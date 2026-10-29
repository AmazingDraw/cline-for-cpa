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

// Dynamic Client Version Engine for Cline with Disk Snapshot Persistence.
//
// Three independent versions, one cache key + probe each (see cline_headers.go
// for the official sources):
//
//	desktop  Cline Desktop app   GitHub releases tag desktop-v*   fallback defaultClientVersion
//	cli      Cline CLI           npm `cline` dist-tags.latest     fallback defaultCLIClientVersion
//	core     @cline/core         npm `@cline/core` latest         fallback defaultCoreVersion
//
// The desktop probe has NO npm fallback: @cline/core is a different version
// line (0.0.95 while Desktop is 0.0.48), and mixing them was the 0.4.15 bug.
//
// Precedence for each header value:
//  1. Explicit config (`client_version` / `core_version`) — always wins.
//  2. max(cache, compiled fallback) by version order. The cache (memory, TTL 6h,
//     persisted to `~/.cli-proxy-api/data/cline-version-cache.json`) lets a running
//     plugin follow upstream without a rebuild; the max() keeps a stale cache
//     from downgrading a freshly built fallback. build.sh enforces fallback ≥ live.

const (
	versionCacheTTL    = 6 * time.Hour
	versionHTTPTimeout = 5 * time.Second
	versionCacheFile   = "cline-version-cache.json"

	targetKeyDesktop = "desktop"
	targetKeyCLI     = "cli"
	targetKeyCore    = "core"
)

// Overridable in tests (httptest) so version probes never hit the public internet.
var (
	githubReleasesURL  = "https://api.github.com/repos/cline/cline/releases?per_page=30"
	npmRegistryBaseURL = "https://registry.npmjs.org"
)

type versionCacheEntry struct {
	Version   string    `json:"version"`
	FetchedAt time.Time `json:"fetched_at"`
}

var (
	versionMu       sync.RWMutex
	versionCache    = map[string]versionCacheEntry{}
	diskLoadedOnce  sync.Once
	customCachePath string // for testing override
)

// isPlausibleVersion checks if the string looks like a sane semver (e.g. 3.0.65 or 0.0.34).
func isPlausibleVersion(v string) bool {
	v = strings.TrimSpace(v)
	if len(v) < 3 || len(v) > 32 {
		return false
	}
	parts := strings.Split(v, ".")
	if len(parts) < 2 {
		return false
	}
	for _, p := range parts[:2] {
		if p == "" || p[0] < '0' || p[0] > '9' {
			return false
		}
	}
	return true
}

func versionSnapshotPath() string {
	if customCachePath != "" {
		return customCachePath
	}
	if fi, err := os.Stat("/app/data"); err == nil && fi.IsDir() {
		return filepath.Join("/app/data", versionCacheFile)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".cli-proxy-api", "data", versionCacheFile)
}

func loadVersionSnapshotFromDisk() {
	p := versionSnapshotPath()
	if p == "" {
		return
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return
	}
	var stored map[string]versionCacheEntry
	if err := json.Unmarshal(raw, &stored); err != nil || stored == nil {
		return
	}
	versionMu.Lock()
	defer versionMu.Unlock()
	for k, v := range stored {
		if isPlausibleVersion(v.Version) {
			versionCache[k] = v
		}
	}
}

func saveVersionSnapshotToDisk() {
	p := versionSnapshotPath()
	if p == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(p), 0o700)

	versionMu.RLock()
	copyMap := make(map[string]versionCacheEntry, len(versionCache))
	for k, v := range versionCache {
		copyMap[k] = v
	}
	versionMu.RUnlock()

	encoded, err := json.MarshalIndent(copyMap, "", "  ")
	if err != nil {
		return
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, encoded, 0o600); err == nil {
		_ = os.Rename(tmp, p)
	}
}

// scheduleVersionRefresh starts at most one background refresh per key. It is
// a variable so tests can turn background probes off: a goroutine outliving
// its test would race with later tests that swap the shared HTTP client.
var scheduleVersionRefresh = func(key string) {
	if _, busy := refreshing.LoadOrStore(key, struct{}{}); busy {
		return
	}
	go func() {
		defer refreshing.Delete(key)
		refreshVersionInBackground(key)
	}()
}

var refreshing sync.Map

// resolveLiveVersion returns max(cache[key], fallback). A stale or missing
// cache entry schedules a background refresh but never blocks the caller.
func resolveLiveVersion(key, fallback string) string {
	diskLoadedOnce.Do(loadVersionSnapshotFromDisk)

	versionMu.RLock()
	entry, ok := versionCache[key]
	versionMu.RUnlock()

	if !ok || entry.Version == "" || time.Since(entry.FetchedAt) >= versionCacheTTL {
		scheduleVersionRefresh(key)
	}
	if ok && entry.Version != "" && compareVersions(entry.Version, fallback) > 0 {
		return entry.Version
	}
	return fallback
}

// compareVersions orders dotted numeric versions ("0.0.48" < "0.0.95" <
// "0.1.0"). A pre-release suffix ("1.2.3-alpha") sorts below the release.
func compareVersions(a, b string) int {
	split := func(v string) ([]int, bool) {
		v = strings.TrimPrefix(strings.TrimSpace(v), "v")
		pre := false
		if i := strings.IndexAny(v, "-+"); i >= 0 {
			pre = v[i] == '-'
			v = v[:i]
		}
		var out []int
		for _, part := range strings.Split(v, ".") {
			n := 0
			for _, c := range part {
				if c < '0' || c > '9' {
					break
				}
				n = n*10 + int(c-'0')
			}
			out = append(out, n)
		}
		return out, pre
	}
	x, xp := split(a)
	y, yp := split(b)
	for i := 0; i < len(x) || i < len(y); i++ {
		var xi, yi int
		if i < len(x) {
			xi = x[i]
		}
		if i < len(y) {
			yi = y[i]
		}
		if xi != yi {
			if xi < yi {
				return -1
			}
			return 1
		}
	}
	switch {
	case xp && !yp:
		return -1
	case !xp && yp:
		return 1
	}
	return 0
}

// refreshVersionInBackground fetches the latest version without blocking callers.
func refreshVersionInBackground(targetKey string) {
	refreshVersionCtx(context.Background(), targetKey)
}

// refreshVersionCtx is refreshVersionInBackground with a caller deadline (the
// startup sync shares one budget across all probes). Returns true when a new
// plausible version was stored.
func refreshVersionCtx(ctx context.Context, targetKey string) bool {
	versionMu.Lock()
	entry, exists := versionCache[targetKey]
	if exists && time.Since(entry.FetchedAt) < 10*time.Minute {
		// Avoid stampeding fetches within 10 minutes on network failure
		versionMu.Unlock()
		return false
	}
	// Mark attempt time
	versionCache[targetKey] = versionCacheEntry{
		Version:   entry.Version,
		FetchedAt: time.Now(),
	}
	versionMu.Unlock()

	var ver string
	var err error

	switch targetKey {
	case targetKeyCLI:
		ver, err = fetchLatestNpmVersionCtx(ctx, "cline")
	case targetKeyCore:
		ver, err = fetchLatestNpmVersionCtx(ctx, "@cline/core")
	case targetKeyDesktop:
		// Desktop app version comes only from GitHub desktop-v* tags. There is
		// deliberately no npm fallback (@cline/core is a different version line);
		// on failure the cache/fallback keeps serving.
		ver, err = fetchLatestDesktopReleaseVersionCtx(ctx)
	default:
		return false
	}

	if err == nil && isPlausibleVersion(ver) {
		versionMu.Lock()
		versionCache[targetKey] = versionCacheEntry{
			Version:   ver,
			FetchedAt: time.Now(),
		}
		versionMu.Unlock()
		saveVersionSnapshotToDisk()
		return true
	}
	return false
}

func versionProbeEndpoints() (github, npm string) {
	versionMu.RLock()
	defer versionMu.RUnlock()
	return githubReleasesURL, npmRegistryBaseURL
}

func fetchLatestDesktopReleaseVersion() (string, error) {
	return fetchLatestDesktopReleaseVersionCtx(context.Background())
}

func fetchLatestDesktopReleaseVersionCtx(parent context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(parent, versionHTTPTimeout)
	defer cancel()

	githubURL, _ := versionProbeEndpoints()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, githubURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("User-Agent", "Mozilla/5.0")

	resp, err := upstreamClient().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github releases HTTP %d", resp.StatusCode)
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err != nil {
		return "", err
	}

	var releases []struct {
		TagName string `json:"tag_name"`
	}
	if err := json.Unmarshal(raw, &releases); err != nil {
		return "", err
	}

	for _, rel := range releases {
		tag := strings.TrimSpace(rel.TagName)
		if strings.HasPrefix(tag, "desktop-v") {
			ver := strings.TrimPrefix(tag, "desktop-v")
			if isPlausibleVersion(ver) {
				return ver, nil
			}
		}
	}
	return "", fmt.Errorf("no desktop-v release tag found")
}

func fetchLatestNpmVersion(pkgName string) (string, error) {
	return fetchLatestNpmVersionCtx(context.Background(), pkgName)
}

func fetchLatestNpmVersionCtx(parent context.Context, pkgName string) (string, error) {
	_, npmBase := versionProbeEndpoints()
	url := fmt.Sprintf("%s/%s", npmBase, pkgName)
	ctx, cancel := context.WithTimeout(parent, versionHTTPTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.npm.install-v1+json; q=1.0, application/json; q=0.8")

	resp, err := upstreamClient().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("npm registry HTTP %d", resp.StatusCode)
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err != nil {
		return "", err
	}

	var meta struct {
		DistTags struct {
			Latest string `json:"latest"`
		} `json:"dist-tags"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		return "", err
	}
	return strings.TrimSpace(meta.DistTags.Latest), nil
}
