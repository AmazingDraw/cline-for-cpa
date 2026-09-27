package plugin

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"cline-for-cpa/plugin/streamguard"
)

const (
	defaultBaseURL = "https://api.cline.bot/api/v1"
	envAPIKey      = "CLINE_API_KEY"
	// defaultHTTPTimeoutSeconds mirrors the official DEFAULT_HTTP_TIMEOUT_MS
	// (auth/cline.ts:45) — control-plane calls (token refresh, quota) are bounded
	// exactly like the official client's. Streaming is not bounded by this: it
	// relies on the Stream Guard plus the per-phase transport timeouts.
	defaultHTTPTimeoutSeconds = 30
	// defaultHostRefreshIntervalSeconds is the wake lead published to the host
	// (see pluginConfig.HostRefreshIntervalSeconds).
	//
	// Why 600 and not something bigger: the host re-asks every 30 seconds while a
	// refresh is pending but has not produced a new token yet
	// (CLIProxyAPI refreshIneffectiveBackoff, sdk/cliproxy/auth/conductor_refresh.go:32),
	// so the wake lead directly sets how long that idle polling lasts — 1800 made
	// it ~25 minutes of no-op calls, 600 keeps it to ~5 minutes (~10 calls/hour)
	// while still leaving an early heartbeat in the log. The token is only ever
	// exchanged once our own 5-minute lead (refreshLeadDefault) is reached, so the
	// lead choice never changes when the refresh actually happens.
	defaultHostRefreshIntervalSeconds = 600
)

// httpTimeout returns the configured upstream timeout, defaulted when unset.
func httpTimeout(cfg pluginConfig) time.Duration {
	if cfg.HTTPTimeoutSeconds > 0 {
		return time.Duration(cfg.HTTPTimeoutSeconds) * time.Second
	}
	return defaultHTTPTimeoutSeconds * time.Second
}

// pluginConfig is the plugins.configs.cline-for-cpa block (JSON or YAML-mapped JSON).
type pluginConfig struct {
	Enabled                           *bool    `yaml:"enabled" json:"enabled"`
	APIKey                            string   `yaml:"api_key" json:"api_key"`
	APIKeys                           []string `yaml:"api_keys" json:"api_keys"`
	BaseURL                           string   `yaml:"base_url" json:"base_url"`
	FirstFrameTimeoutSeconds          int      `yaml:"first_frame_timeout_seconds" json:"first_frame_timeout_seconds"`
	StreamSilenceTimeoutSeconds       int      `yaml:"stream_silence_timeout_seconds" json:"stream_silence_timeout_seconds"`
	StreamHeartbeatOnlyTimeoutSeconds int      `yaml:"stream_heartbeat_only_timeout_seconds" json:"stream_heartbeat_only_timeout_seconds"`

	// Client identity sent upstream, aligned with the Cline desktop client.
	// Configurable so a desktop version bump needs no rebuild.
	ClientType    string `yaml:"client_type" json:"client_type"`
	ClientVersion string `yaml:"client_version" json:"client_version"`
	HTTPReferer   string `yaml:"http_referer" json:"http_referer"`
	XTitle        string `yaml:"x_title" json:"x_title"`
	// Profile is a shortcut that sets a coherent official identity in one go:
	// "cli" → cline-cli + platform cli, "desktop" → platform desktop.
	Profile string `yaml:"profile" json:"profile"`
	// Remaining official identity headers (see plugin/cline_headers.go).
	Platform        string `yaml:"platform" json:"platform"`
	PlatformVersion string `yaml:"platform_version" json:"platform_version"`
	CoreVersion     string `yaml:"core_version" json:"core_version"`
	IsMultiRoot     string `yaml:"is_multi_root" json:"is_multi_root"`
	TaskID          string `yaml:"task_id" json:"task_id"`

	// AuthDir holds the auth-file directory whose OAuth mutations are serialized
	// by the cross-process refresh lock (empty → ~/.cli-proxy-api/auths).
	AuthDir string `yaml:"auth_dir" json:"auth_dir"`
	// ShareDesktopStore additionally takes the official Cline lock
	// (providers.json.oauth-<sha256(provider)>.lock) so this plugin queues
	// behind the desktop app / CLI / hub instead of racing their rotations.
	ShareDesktopStore bool `yaml:"share_desktop_store" json:"share_desktop_store"`
	// HTTPTimeoutSeconds bounds every upstream call this plugin makes.
	HTTPTimeoutSeconds int `yaml:"http_timeout_seconds" json:"http_timeout_seconds"`

	// HostRefreshIntervalSeconds is the "host refresh contract" (0.2.8): the
	// plugin publishes it as Metadata["refresh_interval_seconds"] and as the
	// lead used for the Auth.NextRefreshAfter it reports.
	//
	// Why it exists: the host only refreshes a plugin auth when it knows when to
	// do so. Its built-in providers register a refresh lead
	// (CLIProxyAPI sdk/auth/refresh_registry.go registers codex/claude/
	// antigravity/kimi/xai), and Manager.shouldRefresh() falls back to
	// authPreferredInterval()/ProviderRefreshLead() — both of which are empty for
	// "cline". The consequence was structural, not incidental:
	//
	//   host never refreshes proactively
	//     → the bearer is simply left to expire (1h TTL)
	//     → the next request gets a 401 from upstream
	//     → the host's 401 recovery is the only remaining chance
	//     → if that recovery is judged "unauthorized", the host runs
	//       applyAuthFailureState()/refreshAuthForRequest(): Unavailable=true,
	//       Status=StatusError, and hasUnauthorizedAuthFailure() makes
	//       shouldRefresh() return false forever
	//     → every request then fails in 2–48 ms with 503 auth_unavailable
	//       (non-quota unavailability is blockReasonOther, so the client does
	//       not even get a 429 + Retry-After), i.e. a permanent outage that only
	//       a re-login (or host restart) clears.
	//
	// Publishing a positive interval makes the host treat cline like a built-in
	// provider: it starts polling us before expiry, and our own 5-minute lead
	// (refreshLeadDefault, unchanged for official parity) decides when the token
	// is actually exchanged. The lead is deliberately modest (default 600s) since
	// the host keeps polling every 30s until the exchange succeeds, so a larger
	// lead only adds idle calls. Set 0 to opt out and restore the old behaviour.
	HostRefreshIntervalSeconds *int `yaml:"refresh_interval_seconds" json:"refresh_interval_seconds"`

	// ReasoningEffortNormalize repairs/drops illegal reasoning_effort values,
	// which the upstream answers with a silent empty response. Defaults to true.
	ReasoningEffortNormalize *bool `yaml:"reasoning_effort_normalize" json:"reasoning_effort_normalize"`

	// ForceStreamUpstream makes every upstream call streaming and aggregates the
	// deltas back for non-streaming clients. Cline rejects non-streaming requests
	// outright ("empty response content"), so this defaults to true.
	ForceStreamUpstream *bool `yaml:"force_stream_upstream" json:"force_stream_upstream"`

	// CredentialPreference sets the priority schedule between OAuth and Key
	// credentials: "oauth_first" (default, OAuth priority=1, Key priority=0),
	// "key_first" (Key priority=1, OAuth priority=0), or "round_robin" (both priority=0).
	CredentialPreference string `yaml:"credential_preference" json:"credential_preference"`
}

var (
	configMu     sync.RWMutex
	activeConfig = defaultConfig()
)

func defaultConfig() pluginConfig {
	return pluginConfig{
		BaseURL:                           defaultBaseURL,
		FirstFrameTimeoutSeconds:          int(streamguard.DefaultFirstFrame / time.Second),
		StreamSilenceTimeoutSeconds:       int(streamguard.DefaultSilence / time.Second),
		StreamHeartbeatOnlyTimeoutSeconds: int(streamguard.DefaultHeartbeatOnly / time.Second),
		ClientType:                        defaultClientType,
		ClientVersion:                     defaultClientVersion,
		HTTPReferer:                       defaultHTTPReferer,
		XTitle:                            defaultXTitle,
		HTTPTimeoutSeconds:                defaultHTTPTimeoutSeconds,
		HostRefreshIntervalSeconds:        intPtr(defaultHostRefreshIntervalSeconds),
		ForceStreamUpstream:               boolPtr(true),
		ReasoningEffortNormalize:          boolPtr(true),
		CredentialPreference:              "oauth_first",
	}
}

func boolPtr(v bool) *bool { return &v }

func intPtr(v int) *int { return &v }

// hostRefreshInterval returns the interval published to the host, or 0 when the
// feature is switched off (explicit `refresh_interval_seconds: 0`).
func hostRefreshInterval(cfg pluginConfig) time.Duration {
	if cfg.HostRefreshIntervalSeconds == nil || *cfg.HostRefreshIntervalSeconds <= 0 {
		return 0
	}
	return time.Duration(*cfg.HostRefreshIntervalSeconds) * time.Second
}

// forceStreamUpstream reports whether non-streaming client requests are served
// by internally streaming from the upstream and aggregating the result.
func forceStreamUpstream(cfg pluginConfig) bool {
	return cfg.ForceStreamUpstream == nil || *cfg.ForceStreamUpstream
}

func applyConfig(raw []byte) error {
	cfg := defaultConfig()
	if len(raw) > 0 {
		var incoming pluginConfig
		// Host sends plugins.configs.<id> as YAML bytes (same as cursor-for-cpa).
		// yaml.v3 also accepts JSON.
		if err := yaml.Unmarshal(raw, &incoming); err != nil {
			return fmt.Errorf("parse plugin config: %w", err)
		}
		if incoming.Enabled != nil {
			cfg.Enabled = incoming.Enabled
		}
		if incoming.APIKey != "" {
			cfg.APIKey = incoming.APIKey
		}
		if len(incoming.APIKeys) > 0 {
			cfg.APIKeys = incoming.APIKeys
		}
		if incoming.BaseURL != "" {
			cfg.BaseURL = incoming.BaseURL
		}
		if incoming.FirstFrameTimeoutSeconds > 0 {
			cfg.FirstFrameTimeoutSeconds = incoming.FirstFrameTimeoutSeconds
		}
		if incoming.StreamSilenceTimeoutSeconds > 0 {
			cfg.StreamSilenceTimeoutSeconds = incoming.StreamSilenceTimeoutSeconds
		}
		if incoming.StreamHeartbeatOnlyTimeoutSeconds > 0 {
			cfg.StreamHeartbeatOnlyTimeoutSeconds = incoming.StreamHeartbeatOnlyTimeoutSeconds
		}
		if incoming.ClientType != "" {
			cfg.ClientType = incoming.ClientType
		}
		if incoming.ClientVersion != "" {
			cfg.ClientVersion = incoming.ClientVersion
		}
		if incoming.HTTPReferer != "" {
			cfg.HTTPReferer = incoming.HTTPReferer
		}
		if incoming.XTitle != "" {
			cfg.XTitle = incoming.XTitle
		}
		if incoming.Profile != "" {
			cfg.Profile = incoming.Profile
		}
		if incoming.Platform != "" {
			cfg.Platform = incoming.Platform
		}
		if incoming.PlatformVersion != "" {
			cfg.PlatformVersion = incoming.PlatformVersion
		}
		if incoming.CoreVersion != "" {
			cfg.CoreVersion = incoming.CoreVersion
		}
		if incoming.IsMultiRoot != "" {
			cfg.IsMultiRoot = incoming.IsMultiRoot
		}
		if incoming.TaskID != "" {
			cfg.TaskID = incoming.TaskID
		}
		if incoming.AuthDir != "" {
			cfg.AuthDir = incoming.AuthDir
		}
		cfg.ShareDesktopStore = incoming.ShareDesktopStore
		if incoming.HTTPTimeoutSeconds > 0 {
			cfg.HTTPTimeoutSeconds = incoming.HTTPTimeoutSeconds
		}
		// Pointer semantics: an explicit `refresh_interval_seconds: 0` disables
		// the host refresh contract, absent keeps the default.
		if incoming.HostRefreshIntervalSeconds != nil {
			cfg.HostRefreshIntervalSeconds = incoming.HostRefreshIntervalSeconds
		}
		if incoming.ForceStreamUpstream != nil {
			cfg.ForceStreamUpstream = incoming.ForceStreamUpstream
		}
		if incoming.ReasoningEffortNormalize != nil {
			cfg.ReasoningEffortNormalize = incoming.ReasoningEffortNormalize
		}
		if incoming.CredentialPreference != "" {
			cfg.CredentialPreference = strings.TrimSpace(incoming.CredentialPreference)
		}
	}
	configMu.Lock()
	activeConfig = cfg
	configMu.Unlock()
	syncAuthFilePriorities(cfg, clineAuthDir(cfg))
	return nil
}

func currentConfig() pluginConfig {
	configMu.RLock()
	defer configMu.RUnlock()
	return activeConfig
}

func resolveAPIKeys(cfg pluginConfig) []string {
	seen := make(map[string]struct{})
	var keys []string
	add := func(k string) {
		k = strings.TrimSpace(k)
		if k == "" {
			return
		}
		if _, exists := seen[k]; !exists {
			seen[k] = struct{}{}
			keys = append(keys, k)
		}
	}
	for _, k := range cfg.APIKeys {
		add(k)
	}
	if cfg.APIKey != "" {
		add(cfg.APIKey)
	}
	if env := os.Getenv(envAPIKey); env != "" {
		add(env)
	}
	return keys
}

func resolveAPIKey(cfg pluginConfig) string {
	return ""
}

func streamGuardConfig(cfg pluginConfig) streamguard.Config {
	return streamguard.Config{
		FirstFrame:    time.Duration(cfg.FirstFrameTimeoutSeconds) * time.Second,
		Silence:       time.Duration(cfg.StreamSilenceTimeoutSeconds) * time.Second,
		HeartbeatOnly: time.Duration(cfg.StreamHeartbeatOnlyTimeoutSeconds) * time.Second,
	}
}

const configManagedKeyMarker = "config_api_key"

func priorityForCredential(pref string, isOAuth bool) int {
	pref = strings.ToLower(strings.TrimSpace(pref))
	if pref == "" {
		pref = "oauth_first"
	}
	if isOAuth {
		if pref == "oauth_first" {
			return 1
		}
		return 0
	}
	if pref == "key_first" {
		return 1
	}
	return 0
}

func syncConfigAPIKeyCredential(cfg pluginConfig) {
	// 0.4.0: keys are no longer seeded or recycled. Hygiene still strips
	// leaked api_key fields from OAuth files so a leftover cline-key-*.json
	// cannot pollute a login.
	syncAuthFilePriorities(cfg, clineAuthDir(cfg))
}

func syncAuthFilePriorities(cfg pluginConfig, dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		filePath := filepath.Join(dir, e.Name())
		raw, err := os.ReadFile(filePath)
		if err != nil {
			continue
		}
		var data map[string]any
		if err := json.Unmarshal(raw, &data); err != nil || data == nil {
			continue
		}
		if t, _ := data["type"].(string); !strings.EqualFold(strings.TrimSpace(t), ProviderKey) {
			continue
		}
		refreshToken, _ := data["refresh_token"].(string)
		accessToken, _ := data["access_token"].(string)
		apiKey, _ := data["api_key"].(string)
		hasOAuth := strings.TrimSpace(refreshToken) != "" || strings.TrimSpace(accessToken) != ""
		hasKey := strings.TrimSpace(apiKey) != ""
		if !hasOAuth && !hasKey {
			continue
		}

		targetPriority := priorityForCredential(cfg.CredentialPreference, hasOAuth)
		modified := false

		// Purge accidental OAuth tokens in any cline-key-*.json file
		isKeyFile := strings.HasPrefix(e.Name(), "cline-key-")
		if isKeyFile && hasOAuth {
			delete(data, "refresh_token")
			delete(data, "access_token")
			delete(data, "expires_at")
			hasOAuth = false
			targetPriority = priorityForCredential(cfg.CredentialPreference, false)
			modified = true
		}

		currentPri, hasPri := data["priority"].(float64)
		if !hasPri || int(currentPri) != targetPriority {
			data["priority"] = targetPriority
			modified = true
		}

		// Purge accidental api_key or managed_by leakage in an OAuth auth file
		if hasOAuth && hasKey && !isKeyFile {
			delete(data, "api_key")
			if m, ok := data["metadata"].(map[string]any); ok {
				delete(m, "managed_by")
			}
			modified = true
		}

		if modified {
			if encoded, err := json.MarshalIndent(data, "", "  "); err == nil {
				_ = safeInPlaceWrite(filePath, encoded)
			}
		}
	}
}
