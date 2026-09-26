package plugin

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	modelsCacheFile = "cline-models-cache.json"
	modelsCacheTTL  = 12 * time.Hour
)

type liveFeedResponse struct {
	Recommended []feedModelItem `json:"recommended"`
	Free        []feedModelItem `json:"free"`
	ClinePass   []feedModelItem `json:"clinePass"`
	ClineCloud  []feedModelItem `json:"clineCloud"`
}

type feedModelItem struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
}

type liveCatalogResponse struct {
	Data []catalogModelItem `json:"data"`
}

type catalogModelItem struct {
	ID            string         `json:"id"`
	Name          string         `json:"name"`
	ContextLength int64          `json:"context_length"`
	Architecture  map[string]any `json:"architecture"`
	Supported     []string       `json:"supported_parameters"`
	TopProvider   map[string]any `json:"top_provider"`
	Description   string         `json:"description"`
}

type modelsSnapshot struct {
	FetchedAt time.Time            `json:"fetched_at"`
	Models    map[string]modelMeta `json:"models"`
	Order     []string             `json:"order"`
}

var (
	modelsUpdaterMu      sync.RWMutex
	modelsUpdaterOnce    sync.Once
	modelsDiskLoadedOnce sync.Once
	customModelsPath     string

	dynamicCatalog   = make(map[string]modelMeta)
	dynamicOrder     []string
	lastModelsFetch  time.Time
	modelsInProgress bool
)

func modelsSnapshotPath() string {
	if customModelsPath != "" {
		return customModelsPath
	}
	if fi, err := os.Stat("/app/data"); err == nil && fi.IsDir() {
		return filepath.Join("/app/data", modelsCacheFile)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".cli-proxy-api", "data", modelsCacheFile)
}

func loadModelsSnapshotFromDisk() {
	p := modelsSnapshotPath()
	if p == "" {
		return
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return
	}
	var snap modelsSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil || len(snap.Models) == 0 {
		return
	}

	modelsUpdaterMu.Lock()
	defer modelsUpdaterMu.Unlock()
	dynamicCatalog = snap.Models
	dynamicOrder = snap.Order
	lastModelsFetch = snap.FetchedAt
}

func saveModelsSnapshotToDisk(snap modelsSnapshot) {
	p := modelsSnapshotPath()
	if p == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(p), 0o700)

	encoded, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, encoded, 0o600); err == nil {
		_ = os.Rename(tmp, p)
	}
}

// StartModelsUpdater starts background polling of recommended models and catalog.
func StartModelsUpdater() {
	modelsUpdaterOnce.Do(func() {
		modelsDiskLoadedOnce.Do(loadModelsSnapshotFromDisk)
		go func() {
			triggerModelsRefresh()
			ticker := time.NewTicker(6 * time.Hour)
			defer ticker.Stop()
			for range ticker.C {
				triggerModelsRefresh()
			}
		}()
	})
}

func triggerModelsRefresh() {
	modelsUpdaterMu.Lock()
	if modelsInProgress || time.Since(lastModelsFetch) < modelsCacheTTL {
		modelsUpdaterMu.Unlock()
		return
	}
	modelsInProgress = true
	modelsUpdaterMu.Unlock()

	defer func() {
		modelsUpdaterMu.Lock()
		modelsInProgress = false
		modelsUpdaterMu.Unlock()
	}()

	cfg := currentConfig()
	token := resolveAPIKey(cfg)
	if token == "" {
		dir := clineAuthDir(cfg)
		token = firstAccessTokenFromDir(dir)
	}

	snap, err := fetchLiveModelsSnapshot(cfg.BaseURL, token)
	if err != nil {
		return
	}

	modelsUpdaterMu.Lock()
	dynamicCatalog = snap.Models
	dynamicOrder = snap.Order
	lastModelsFetch = snap.FetchedAt
	modelsUpdaterMu.Unlock()

	saveModelsSnapshotToDisk(snap)
	rebuildModelIndex()
}

func firstAccessTokenFromDir(dir string) string {
	if dir == "" {
		return ""
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "cline-*.json"))
	for _, m := range matches {
		raw, err := os.ReadFile(m)
		if err != nil {
			continue
		}
		var doc struct {
			AccessToken string `json:"access_token"`
		}
		if json.Unmarshal(raw, &doc) == nil && strings.TrimSpace(doc.AccessToken) != "" {
			return strings.TrimSpace(doc.AccessToken)
		}
	}
	return ""
}

func fetchLiveModelsSnapshot(baseURL, token string) (modelsSnapshot, error) {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	client := &http.Client{Timeout: 30 * time.Second}

	// 1. Fetch recommended models
	reqFeed, err := http.NewRequest(http.MethodGet, baseURL+"/ai/cline/recommended-models", nil)
	if err != nil {
		return modelsSnapshot{}, err
	}
	setDiscoveryHeaders(reqFeed, token)
	respFeed, err := client.Do(reqFeed)
	if err != nil {
		return modelsSnapshot{}, err
	}
	defer respFeed.Body.Close()
	if respFeed.StatusCode >= 400 {
		return modelsSnapshot{}, fmt.Errorf("feed status %d", respFeed.StatusCode)
	}
	var feed liveFeedResponse
	if err := json.NewDecoder(respFeed.Body).Decode(&feed); err != nil {
		return modelsSnapshot{}, err
	}

	// 2. Fetch full catalog (optional, best-effort for context windows and parameters)
	byLeaf := make(map[string][]catalogModelItem)
	reqCat, err := http.NewRequest(http.MethodGet, baseURL+"/ai/cline/models", nil)
	if err == nil {
		setDiscoveryHeaders(reqCat, token)
		if respCat, err := client.Do(reqCat); err == nil && respCat.StatusCode == http.StatusOK {
			defer respCat.Body.Close()
			body, _ := io.ReadAll(io.LimitReader(respCat.Body, 16<<20))
			var cat liveCatalogResponse
			if json.Unmarshal(body, &cat) == nil {
				for _, c := range cat.Data {
					leaf := leafOfModel(c.ID)
					byLeaf[leaf] = append(byLeaf[leaf], c)
				}
			}
		}
	}

	catalogMap := make(map[string]modelMeta)
	var order []string

	processTier := func(tierName string, items []feedModelItem) {
		for _, item := range items {
			id := strings.TrimSpace(item.ID)
			if id == "" {
				continue
			}
			order = append(order, id)
			leaf := leafOfModel(strings.TrimSuffix(id, ":free"))

			meta := modelMeta{
				Tier:        tierName,
				DisplayName: item.Name,
				Description: item.Description,
			}

			// Merge catalog metadata if available
			if cands, ok := byLeaf[leaf]; ok && len(cands) > 0 {
				cand := cands[0]
				meta.Underlying = cand.ID
				meta.ContextLength = cand.ContextLength
				meta.MaxOutputTokens = int64Field(cand.TopProvider, "max_completion_tokens")
				meta.InputModalities = stringSliceField(cand.Architecture, "input_modalities")
				meta.OutputModalities = stringSliceField(cand.Architecture, "output_modalities")
				meta.Parameters = cand.Supported
				if meta.Description == "" {
					meta.Description = strings.TrimSpace(cand.Description)
				}
			} else if staticMeta, ok := generatedCatalog[id]; ok {
				// Fallback to static catalog fields for known models
				meta.Underlying = staticMeta.Underlying
				meta.ContextLength = staticMeta.ContextLength
				meta.MaxOutputTokens = staticMeta.MaxOutputTokens
				meta.InputModalities = staticMeta.InputModalities
				meta.OutputModalities = staticMeta.OutputModalities
				meta.Parameters = staticMeta.Parameters
			}

			// Sane defaults for brand new models if catalog lacked them
			if meta.ContextLength <= 0 {
				meta.ContextLength = 1048576
			}
			if meta.MaxOutputTokens <= 0 {
				meta.MaxOutputTokens = 65536
			}
			if len(meta.InputModalities) == 0 {
				meta.InputModalities = []string{"text", "image"}
			}
			if len(meta.OutputModalities) == 0 {
				meta.OutputModalities = []string{"text"}
			}

			catalogMap[id] = meta
		}
	}

	processTier("recommended", feed.Recommended)
	processTier("free", feed.Free)
	processTier("clinePass", feed.ClinePass)
	processTier("clineCloud", feed.ClineCloud)

	return modelsSnapshot{
		FetchedAt: time.Now(),
		Models:    catalogMap,
		Order:     order,
	}, nil
}

func setDiscoveryHeaders(req *http.Request, token string) {
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("User-Agent", "Cline/"+defaultClientVersion)
	req.Header.Set("X-Client-Type", "cline-desktop")
	req.Header.Set("X-Client-Version", defaultClientVersion)
	req.Header.Set("X-Platform", "desktop")
	req.Header.Set("X-Platform-Version", defaultClientVersion)
	req.Header.Set("X-Core-Version", defaultClientVersion)
	req.Header.Set("HTTP-Referer", defaultHTTPReferer)
	req.Header.Set("X-Title", defaultXTitle)
}

func int64Field(m map[string]any, key string) int64 {
	if m == nil {
		return 0
	}
	switch v := m[key].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	}
	return 0
}

func stringSliceField(m map[string]any, key string) []string {
	if m == nil {
		return nil
	}
	raw, ok := m[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if s, ok := item.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}
