package plugin

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestIsPlausibleVersion(t *testing.T) {
	valid := []string{"3.0.65", "0.0.34", "0.0.86", "1.2.3-alpha.1"}
	for _, v := range valid {
		if !isPlausibleVersion(v) {
			t.Fatalf("expected %q to be plausible version", v)
		}
	}

	invalid := []string{"", "1", "abc", "v", "...", "99999999999999999999999999999999999"}
	for _, v := range invalid {
		if isPlausibleVersion(v) {
			t.Fatalf("expected %q to be implausible version", v)
		}
	}
}

func TestResolveLiveClientVersionPrecedence(t *testing.T) {
	// 1. Explicit user config wins
	cfg := pluginConfig{
		ClientVersion: "9.9.99",
	}
	h := resolveClineHeaders(cfg)
	if h.Version != "9.9.99" {
		t.Fatalf("expected user config version 9.9.99, got %q", h.Version)
	}
	if h.PlatformVer != "9.9.99" || h.CoreVersion != "9.9.99" {
		t.Fatalf("expected 3-header alignment with 9.9.99, got plat=%q core=%q", h.PlatformVer, h.CoreVersion)
	}

	// 2. Cached dynamic version is used when user config is empty
	versionMu.Lock()
	versionCache["desktop"] = versionCacheEntry{
		Version:   "0.0.34",
		FetchedAt: time.Now(),
	}
	versionMu.Unlock()

	cfgEmpty := pluginConfig{
		ClientType: "cline-desktop",
	}
	h2 := resolveClineHeaders(cfgEmpty)
	if h2.Version != "0.0.34" {
		t.Fatalf("expected dynamic version 0.0.34, got %q", h2.Version)
	}
	if h2.PlatformVer != "0.0.34" || h2.CoreVersion != "0.0.34" {
		t.Fatalf("expected 3-header alignment with 0.0.34, got plat=%q core=%q", h2.PlatformVer, h2.CoreVersion)
	}

	// 3. CLI profile resolves to "cli" key
	versionMu.Lock()
	versionCache["cli"] = versionCacheEntry{
		Version:   "3.0.99",
		FetchedAt: time.Now(),
	}
	versionMu.Unlock()

	cfgCLI := pluginConfig{
		Profile: "cli",
	}
	h3 := resolveClineHeaders(cfgCLI)
	if h3.Version != "3.0.99" {
		t.Fatalf("expected CLI dynamic version 3.0.99, got %q", h3.Version)
	}
	if h3.ClientType != "cline-cli" || h3.Platform != "cli" {
		t.Fatalf("expected cli profile types, got type=%q plat=%q", h3.ClientType, h3.Platform)
	}

	// Clean up test cache
	versionMu.Lock()
	delete(versionCache, "desktop")
	delete(versionCache, "cli")
	versionMu.Unlock()
}

func TestDiskSnapshotPersistenceAndColdStart(t *testing.T) {
	tempDir := t.TempDir()
	snapshotFile := filepath.Join(tempDir, "test-version-cache.json")
	customCachePath = snapshotFile
	defer func() { customCachePath = "" }()

	// Populate cache and save snapshot to disk
	versionMu.Lock()
	versionCache["desktop"] = versionCacheEntry{
		Version:   "0.0.35",
		FetchedAt: time.Now(),
	}
	versionCache["cli"] = versionCacheEntry{
		Version:   "3.1.0",
		FetchedAt: time.Now(),
	}
	versionMu.Unlock()

	saveVersionSnapshotToDisk()

	if _, err := os.Stat(snapshotFile); err != nil {
		t.Fatalf("expected snapshot file to exist at %s: %v", snapshotFile, err)
	}

	// Wipe memory cache entirely (simulating power off / machine reboot)
	versionMu.Lock()
	versionCache = map[string]versionCacheEntry{}
	versionMu.Unlock()

	// Load snapshot from disk
	loadVersionSnapshotFromDisk()

	versionMu.RLock()
	coreEntry := versionCache["desktop"]
	cliEntry := versionCache["cli"]
	versionMu.RUnlock()

	if coreEntry.Version != "0.0.35" {
		t.Fatalf("expected desktop version 0.0.35 recovered from disk, got %q", coreEntry.Version)
	}
	if cliEntry.Version != "3.1.0" {
		t.Fatalf("expected cli version 3.1.0 recovered from disk, got %q", cliEntry.Version)
	}

	// Clean up
	versionMu.Lock()
	versionCache = map[string]versionCacheEntry{}
	versionMu.Unlock()
}

func TestApplyClineHeadersInjectsAllAlignedHeaders(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://api.cline.bot", nil)
	applyClineHeaders(req, pluginConfig{
		ClientVersion: "0.0.34",
	})

	if req.Header.Get("User-Agent") != "Cline/0.0.34" {
		t.Fatalf("unexpected User-Agent: %s", req.Header.Get("User-Agent"))
	}
	if req.Header.Get("X-Client-Version") != "0.0.34" {
		t.Fatalf("unexpected X-Client-Version: %s", req.Header.Get("X-Client-Version"))
	}
	if req.Header.Get("X-Platform-Version") != "0.0.34" {
		t.Fatalf("unexpected X-Platform-Version: %s", req.Header.Get("X-Platform-Version"))
	}
	if req.Header.Get("X-Core-Version") != "0.0.34" {
		t.Fatalf("unexpected X-Core-Version: %s", req.Header.Get("X-Core-Version"))
	}
}
