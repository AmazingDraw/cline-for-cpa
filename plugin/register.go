package plugin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	PluginName  = "cline-for-cpa"
	PluginTitle = "Cline" // management-panel display name (id stays PluginName)
	ProviderKey = "cline"
)

// PluginVersion is a var, not a const, so build.sh can stamp it with
// -ldflags "-X cline-for-cpa/plugin.PluginVersion=…". A const silently ignores
// -X (verified: `go build -ldflags "-X main.V=ZZZ"` on a const is a no-op), which
// would let a PLUGIN_VERSION override produce a file named 9.9.9 that still
// reports 0.3.20 to the host — the "silent lie" build.sh's own comment warns about.
// The default below stays the single source of truth when the flag is absent.
var PluginVersion = "0.4.0"

// HandleMethod is the plugin ABI dispatcher (mirrors cursor-for-cpa plugin.HandleMethod).
func HandleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		StartVersionUpdater()
		StartModelsUpdater()
		return handleLifecycle(request)
	case "plugin.shutdown":
		return okEnvelope(map[string]any{})
	case "executor.identifier", "auth.identifier":
		return okEnvelope(map[string]string{"identifier": ProviderKey})
	case "auth.parse":
		return handleAuthParse(request)
	case "auth.login.start":
		return handleAuthLoginStart(request)
	case "auth.login.poll":
		return handleAuthLoginPoll(request)
	case "auth.refresh":
		return handleAuthRefresh(request)
	case "model.static":
		return okEnvelope(staticModels())
	case "model.for_auth":
		return handleModelForAuth(request)
	case "quota.identifier":
		return handleQuotaIdentifier(request)
	case "quota.describe":
		return handleQuotaDescribe(request)
	case "quota.fetch":
		return handleQuotaFetch(request)
	case "quota.reset":
		return handleQuotaReset(request)
	case "executor.execute":
		return handleExecute(request, false)
	case "executor.execute_stream":
		return handleExecute(request, true)
	case "executor.count_tokens":
		return okEnvelope(map[string]any{"Payload": []byte(`{"prompt_tokens":0}`)})
	case "executor.http_request":
		return ErrorEnvelope("not_implemented", "cline-for-cpa does not support raw HTTP passthrough"), nil
	default:
		return ErrorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

type lifecycleRequest struct {
	ConfigJSON []byte `json:"config_json"`
	ConfigYAML []byte `json:"config_yaml"` // accepted as JSON bytes if host sends YAML-as-JSON
}

func handleLifecycle(request []byte) ([]byte, error) {
	if len(request) > 0 {
		var req lifecycleRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, fmt.Errorf("decode lifecycle: %w", err)
		}
		raw := req.ConfigYAML
		if len(raw) == 0 {
			raw = req.ConfigJSON
		}
		if len(raw) > 0 {
			if err := applyConfig(raw); err != nil {
				return ErrorEnvelope("invalid_config", err.Error()), nil
			}
		}
	}
	return okEnvelope(registration())
}

func registration() map[string]any {
	cfg := currentConfig()
	return map[string]any{
		"schema_version": 1,
		"metadata": map[string]any{
			"Name":             PluginTitle,
			"Version":          PluginVersion,
			"Author":           "ShuaiHui",
			"GitHubRepository": "https://github.com/AmazingDraw/cline-for-cpa",
			"Logo":             PluginLogo,
			"ConfigFields": []map[string]any{
				{"name": "base_url", "type": "string", "description": "Upstream OpenAI-compatible base (default https://api.cline.bot/api/v1)"},
				{"name": "refresh_interval_seconds", "type": "number", "description": "Host refresh contract: seconds before expiry the host is told to poll this auth (default 600, 0 disables). Without it the host never refreshes cline proactively and the token is left to expire"},
				{"name": "first_frame_timeout_seconds", "type": "number", "description": fmt.Sprintf("First-frame timeout (default %d)", cfg.FirstFrameTimeoutSeconds)},
				{"name": "stream_silence_timeout_seconds", "type": "number", "description": fmt.Sprintf("Inbound silence timeout (default %d)", cfg.StreamSilenceTimeoutSeconds)},
				{"name": "stream_heartbeat_only_timeout_seconds", "type": "number", "description": fmt.Sprintf("Heartbeat-only timeout (default %d)", cfg.StreamHeartbeatOnlyTimeoutSeconds)},
			},
		},
		"capabilities": map[string]any{
			"model_provider":          true,
			"auth_provider":           true,
			"quota_provider":          true,
			"executor":                true,
			"executor_model_scope":    "oauth",
			"executor_input_formats":  []string{"chat-completions"},
			"executor_output_formats": []string{"chat-completions"},
		},
	}
}

// abiModelInfo mirrors CLIProxyAPI pluginapi.ModelInfo field names (no JSON tags).
// The host ABI round-trips via encoding/json into untagged structs, so snake_case
// keys like "owned_by" do NOT populate OwnedBy — only PascalCase survives.
type abiModelInfo struct {
	ID      string
	Object  string
	OwnedBy string
	Type    string

	// Display + capability metadata. The host's pluginapi.ModelInfo carries all
	// of these (DisplayName/Name/Description/InputTokenLimit/OutputTokenLimit/
	// ContextLength/MaxCompletionTokens/Supported*Modalities/SupportedParameters),
	// and the panel and window logic read them — so they are filled from the
	// generated catalog instead of being left at zero.
	DisplayName               string
	Name                      string
	Description               string
	InputTokenLimit           int64
	OutputTokenLimit          int64
	ContextLength             int64
	MaxCompletionTokens       int64
	SupportedInputModalities  []string
	SupportedOutputModalities []string
	SupportedParameters       []string
}

// abiModelResponse mirrors pluginapi.ModelResponse.
type abiModelResponse struct {
	Provider string
	Models   []abiModelInfo
}

func staticModels() abiModelResponse {
	return modelsForFreeTier(false)
}

func handleModelForAuth(request []byte) ([]byte, error) {
	if len(request) == 0 {
		return okEnvelope(staticModels())
	}
	var req struct {
		AuthID         string            `json:"AuthID"`
		AuthIDLow      string            `json:"auth_id"`
		StorageJSON    []byte            `json:"StorageJSON"`
		StorageJSONLow []byte            `json:"storage_json"`
		Metadata       map[string]any    `json:"Metadata"`
		MetadataLow    map[string]any    `json:"metadata"`
		Attributes     map[string]string `json:"Attributes"`
		AttributesLow  map[string]string `json:"attributes"`
	}
	_ = json.Unmarshal(request, &req)

	storageJSON := req.StorageJSON
	if len(storageJSON) == 0 {
		storageJSON = req.StorageJSONLow
	}
	metadata := req.Metadata
	if len(metadata) == 0 {
		metadata = req.MetadataLow
	}
	attributes := req.Attributes
	if len(attributes) == 0 {
		attributes = req.AttributesLow
	}
	authID := firstNonEmptyString(req.AuthID, req.AuthIDLow)

	isFree := isFreeTierCredential(authID, storageJSON, metadata, attributes)
	return okEnvelope(modelsForFreeTier(isFree))
}

type tierCacheEntry struct {
	isPass    bool
	checkedAt time.Time
}

const (
	freeTierCacheTTL = 30 * time.Minute // 免费凭证 30 分钟过期，便于感知用户升级 Cline Pass
	passTierCacheTTL = 6 * time.Hour    // 付费凭证 6 小时过期
)

var (
	credentialTierCacheMu sync.RWMutex
	credentialTierCache   = make(map[string]tierCacheEntry) // token or key -> tierCacheEntry
)

// InvalidateTierCache 允许外部（如 token 刷新、换票、配额刷新）主动清空指定 token 的缓存
func InvalidateTierCache(token string) {
	token = strings.TrimSpace(token)
	if token == "" {
		return
	}
	credentialTierCacheMu.Lock()
	delete(credentialTierCache, token)
	credentialTierCacheMu.Unlock()
}

func isPassAccountRemote(bearer string) bool {
	bearer = strings.TrimSpace(bearer)
	if bearer == "" {
		return false
	}
	credentialTierCacheMu.RLock()
	entry, ok := credentialTierCache[bearer]
	credentialTierCacheMu.RUnlock()
	if ok {
		ttl := freeTierCacheTTL
		if entry.isPass {
			ttl = passTierCacheTTL
		}
		if time.Since(entry.checkedAt) <= ttl {
			return entry.isPass
		}
	}

	req, err := http.NewRequest(http.MethodGet, defaultBaseURL+"/users/me/plan", nil)
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	applyClineHeaders(req, currentConfig())
	req.Header.Set("Accept", "application/json")

	client := upstreamClient()
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	isPass := resp.StatusCode == http.StatusOK
	credentialTierCacheMu.Lock()
	credentialTierCache[bearer] = tierCacheEntry{isPass: isPass, checkedAt: time.Now()}
	credentialTierCacheMu.Unlock()
	return isPass
}

func isFreeTierCredential(authID string, storageJSON []byte, metadata map[string]any, attributes map[string]string) bool {
	// 1. Explicit metadata / attribute plan marker (manual overrides take top priority)
	checkMap := func(m map[string]any) (bool, bool) {
		if m == nil {
			return false, false
		}
		for _, k := range []string{"tier", "plan", "plan_type", "subscription"} {
			if v, ok := m[k].(string); ok {
				vLower := strings.ToLower(v)
				if strings.Contains(vLower, "free") {
					return true, true
				}
				if strings.Contains(vLower, "pass") || strings.Contains(vLower, "pro") {
					return false, true
				}
			}
		}
		return false, false
	}

	if free, found := checkMap(metadata); found {
		return free
	}
	if len(attributes) > 0 {
		attrAny := make(map[string]any, len(attributes))
		for k, v := range attributes {
			attrAny[k] = v
		}
		if free, found := checkMap(attrAny); found {
			return free
		}
	}

	// 2. Extract active credential token (access_token or api_key)
	var bearerToken string

	if len(storageJSON) > 0 {
		var st clineOAuthStorage
		if err := json.Unmarshal(storageJSON, &st); err == nil {
			if free, found := checkMap(st.Metadata); found {
				return free
			}
			if strings.TrimSpace(st.AccessToken) != "" {
				bearerToken = strings.TrimSpace(st.AccessToken)
			}
		}
	}

	if bearerToken == "" && metadata != nil {
		if acc, ok := metadata["access_token"].(string); ok && strings.TrimSpace(acc) != "" {
			bearerToken = strings.TrimSpace(acc)
		}
	}

	if bearerToken == "" && authID != "" {
		cfg := currentConfig()
		dir := clineAuthDir(cfg)
		if dir != "" {
			filePath := filepath.Join(dir, authID)
			if raw, err := os.ReadFile(filePath); err == nil {
				var st clineOAuthStorage
				if json.Unmarshal(raw, &st) == nil {
					if free, found := checkMap(st.Metadata); found {
						return free
					}
					if strings.TrimSpace(st.AccessToken) != "" {
						bearerToken = strings.TrimSpace(st.AccessToken)
					}
				}
			}
		}
	}

	// 3. 权威判定真源：无论是 OAuth access_token 还是 API Key，均统一向官方 /users/me/plan 发起探活
	//    HTTP 200 = 具备有效 Cline Pass 套餐；HTTP 404 = 普通免费凭证
	if bearerToken != "" {
		if isPassAccountRemote(bearerToken) {
			return false // 有效 Cline Pass 付费凭证
		}
	}

	// 4. 未能探活到 Pass 套餐的，全部按免费凭证（Free Tier）安全收敛
	return true
}

func modelsForFreeTier(isFree bool) abiModelResponse {
	advertised := exposedModels()
	models := make([]abiModelInfo, 0, len(advertised))
	for _, m := range advertised {
		if isFree && strings.HasPrefix(m.ID, namespacePass) {
			continue
		}
		models = append(models, abiModelInfo{
			ID:      m.ID,
			Object:  "model",
			OwnedBy: ProviderKey,
			Type:    ProviderKey,

			DisplayName:               m.Meta.DisplayName,
			Name:                      firstNonEmptyString(m.Meta.Underlying, m.ID),
			Description:               m.Meta.Description,
			InputTokenLimit:           m.Meta.ContextLength,
			OutputTokenLimit:          m.Meta.MaxOutputTokens,
			ContextLength:             m.Meta.ContextLength,
			MaxCompletionTokens:       m.Meta.MaxOutputTokens,
			SupportedInputModalities:  m.Meta.InputModalities,
			SupportedOutputModalities: m.Meta.OutputModalities,
			SupportedParameters:       m.Meta.Parameters,
		})
	}
	return abiModelResponse{Provider: ProviderKey, Models: models}
}

// firstNonEmptyString returns the first non-blank value (local helper so the
// ABI file stays free of the credential-oriented helpers).
func firstNonEmptyString(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}
