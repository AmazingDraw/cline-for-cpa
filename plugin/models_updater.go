package plugin

import (
	"context"
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
	// modelsCacheTTL is how old the disk cache may be before plugin.register
	// fetches the list synchronously (see startup_sync.go). It used to be 12h
	// behind a 6h ticker whose result never reached the host.
	modelsCacheTTL = time.Hour
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
	modelsDiskLoadedOnce sync.Once
	customModelsPath     string

	dynamicCatalog  = make(map[string]modelMeta)
	dynamicOrder    []string
	lastModelsFetch time.Time
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

// syncModelsAtStartup refreshes the model list once, synchronously, and swaps
// it into the in-memory index before the caller returns. The caller owns the
// deadline (ctx); a fetch that fails or misses it leaves the disk cache /
// compiled catalog serving. It returns true only when a fresh list was applied.
func syncModelsAtStartup(ctx context.Context) bool {
	token := firstAccessTokenFromDir(clineAuthDir(currentConfig()))
	snap, err := fetchLiveModelsSnapshotCtx(ctx, clineBaseURL(), token)
	if err != nil || len(snap.Models) == 0 {
		return false
	}

	modelsUpdaterMu.Lock()
	dynamicCatalog = snap.Models
	dynamicOrder = snap.Order
	lastModelsFetch = snap.FetchedAt
	modelsUpdaterMu.Unlock()

	saveModelsSnapshotToDisk(snap)
	rebuildModelIndex()
	return true
}

// modelsCacheFresh reports whether the loaded cache is younger than the TTL.
func modelsCacheFresh() bool {
	modelsUpdaterMu.RLock()
	defer modelsUpdaterMu.RUnlock()
	return !lastModelsFetch.IsZero() && time.Since(lastModelsFetch) < modelsCacheTTL
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
			base := filepath.Base(m)
			if strings.HasPrefix(base, "cline-key-") {
				continue
			}
			return strings.TrimSpace(doc.AccessToken)
		}
	}
	return ""
}

// liveFetchTimeout bounds the non-startup entry point (tests / callers without
// their own deadline).
const liveFetchTimeout = 30 * time.Second

func fetchLiveModelsSnapshot(baseURL, token string) (modelsSnapshot, error) {
	ctx, cancel := context.WithTimeout(context.Background(), liveFetchTimeout)
	defer cancel()
	return fetchLiveModelsSnapshotCtx(ctx, baseURL, token)
}

// fetchLiveModelsSnapshotCtx fetches the recommended feed (required) and the
// full catalog (best effort: context windows / parameters) IN PARALLEL, both
// bound by ctx. The catalog is the slow one (~500 KB, 2-4 s), so running it
// after the feed would eat most of a 6 s budget on its own.
func fetchLiveModelsSnapshotCtx(ctx context.Context, baseURL, token string) (modelsSnapshot, error) {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	client := upstreamClient()

	byLeaf := make(map[string][]catalogModelItem)
	catDone := make(chan struct{})
	go func() {
		defer close(catDone)
		reqCat, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/ai/cline/models", nil)
		if err != nil {
			return
		}
		setDiscoveryHeaders(reqCat, token)
		respCat, err := client.Do(reqCat)
		if err != nil {
			return
		}
		defer respCat.Body.Close()
		if respCat.StatusCode != http.StatusOK {
			return
		}
		body, err := io.ReadAll(io.LimitReader(respCat.Body, 16<<20))
		if err != nil {
			return // truncated by the deadline: ignore the partial body
		}
		var cat liveCatalogResponse
		if json.Unmarshal(body, &cat) == nil {
			for _, c := range cat.Data {
				leaf := leafOfModel(c.ID)
				byLeaf[leaf] = append(byLeaf[leaf], c)
			}
		}
	}()

	feed, err := fetchLiveFeed(ctx, client, baseURL, token)
	if err != nil {
		<-catDone // goroutine exits on ctx; never leak it past this call
		return modelsSnapshot{}, err
	}
	<-catDone // happens-before for byLeaf; bounded by ctx

	// Meta for ids the catalog did not cover (catalog slow/down): last known
	// list first, then the compiled table.
	modelsUpdaterMu.RLock()
	prev := make(map[string]modelMeta, len(dynamicCatalog))
	for k, v := range dynamicCatalog {
		prev[k] = v
	}
	modelsUpdaterMu.RUnlock()

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
			} else if staticMeta, ok := knownMeta(prev, id); ok {
				// Catalog missing this id: reuse last known / compiled fields
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
	// Same identity as chat requests (one source of truth for versions).
	applyClineHeaders(req, currentConfig())
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

func fetchLiveFeed(ctx context.Context, client *http.Client, baseURL, token string) (liveFeedResponse, error) {
	var feed liveFeedResponse
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/ai/cline/recommended-models", nil)
	if err != nil {
		return feed, err
	}
	setDiscoveryHeaders(req, token)
	resp, err := client.Do(req)
	if err != nil {
		return feed, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return feed, fmt.Errorf("feed status %d", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&feed); err != nil {
		return feed, err
	}
	return feed, nil
}

// knownMeta looks an id up in the previously loaded list, then the compiled one.
func knownMeta(prev map[string]modelMeta, id string) (modelMeta, bool) {
	if m, ok := prev[id]; ok {
		return m, true
	}
	m, ok := generatedCatalog[id]
	return m, ok
}
