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
// Aligned with official sources:
// - Desktop releases: GitHub API (repos/cline/cline/releases -> desktop-v*)
//   Fallback to NPM (@cline/core) or hardcoded baseline.
// - CLI releases: NPM registry (cline package -> dist-tags.latest).
//
// Multi-tier Fallback & Self-Evolution:
// 1. User config in config.yaml (`client_version`) -> ALWAYS highest precedence.
// 2. In-memory dynamic cache (TTL 6 hours).
// 3. Persistent disk snapshot (`~/.cli-proxy-api/data/cline-version-cache.json`)
//    Automatically loaded on cold start so that even if the machine boots without
//    internet, the baseline is already the latest version from previous online runs.
// 4. Hardcoded baseline (`0.0.34`) as ultimate mechanical safety net.

const (
	githubReleasesURL  = "https://api.github.com/repos/cline/cline/releases?per_page=10"
	npmRegistryBaseURL = "https://registry.npmjs.org"
	versionCacheTTL    = 6 * time.Hour
	versionHTTPTimeout = 5 * time.Second
	versionCacheFile   = "cline-version-cache.json"

	targetKeyDesktop = "desktop"
	targetKeyCLI     = "cli"
)

type versionCacheEntry struct {
	Version   string    `json:"version"`
	FetchedAt time.Time `json:"fetched_at"`
}

var (
	versionMu       sync.RWMutex
	versionCache    = map[string]versionCacheEntry{}
	updaterOnce     sync.Once
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

// resolveLiveClientVersion returns the dynamically detected version for the clientType/profile,
// with multi-level fallback (Memory -> Disk Snapshot -> Hardcoded Baseline).
func resolveLiveClientVersion(clientType, fallback string) string {
	diskLoadedOnce.Do(loadVersionSnapshotFromDisk)

	targetKey := targetKeyDesktop
	ct := strings.ToLower(clientType)
	if strings.HasSuffix(ct, "-cli") || ct == "cli" || strings.Contains(ct, "cline-cli") {
		targetKey = targetKeyCLI
	}

	versionMu.RLock()
	entry, ok := versionCache[targetKey]
	versionMu.RUnlock()

	if ok && entry.Version != "" && time.Since(entry.FetchedAt) < versionCacheTTL {
		return entry.Version
	}

	// Trigger async background refresh if expired or not yet cached
	go refreshVersionInBackground(targetKey)

	if ok && entry.Version != "" {
		return entry.Version
	}
	return fallback
}

// refreshVersionInBackground fetches the latest version without blocking callers.
func refreshVersionInBackground(targetKey string) {
	versionMu.Lock()
	entry, exists := versionCache[targetKey]
	if exists && time.Since(entry.FetchedAt) < 10*time.Minute {
		// Avoid stampeding fetches within 10 minutes on network failure
		versionMu.Unlock()
		return
	}
	// Mark attempt time
	versionCache[targetKey] = versionCacheEntry{
		Version:   entry.Version,
		FetchedAt: time.Now(),
	}
	versionMu.Unlock()

	var ver string
	var err error

	if targetKey == targetKeyCLI {
		ver, err = fetchLatestNpmVersion("cline")
	} else {
		// For desktop, primary source is GitHub releases (desktop-v*)
		ver, err = fetchLatestDesktopReleaseVersion()
		if err != nil || !isPlausibleVersion(ver) {
			// Fallback: npm @cline/core
			ver, err = fetchLatestNpmVersion("@cline/core")
		}
	}

	if err == nil && isPlausibleVersion(ver) {
		versionMu.Lock()
		versionCache[targetKey] = versionCacheEntry{
			Version:   ver,
			FetchedAt: time.Now(),
		}
		versionMu.Unlock()
		saveVersionSnapshotToDisk()
	}
}

func fetchLatestDesktopReleaseVersion() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), versionHTTPTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, githubReleasesURL, nil)
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
	url := fmt.Sprintf("%s/%s", npmRegistryBaseURL, pkgName)
	ctx, cancel := context.WithTimeout(context.Background(), versionHTTPTimeout)
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

// StartVersionUpdater warms up the version cache on startup.
func StartVersionUpdater() {
	diskLoadedOnce.Do(loadVersionSnapshotFromDisk)
	updaterOnce.Do(func() {
		go func() {
			refreshVersionInBackground(targetKeyDesktop)
			refreshVersionInBackground(targetKeyCLI)
		}()
	})
}
