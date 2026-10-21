package plugin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	resetVersionCacheForTest(t)

	// 1. Explicit user config wins for the client version; core stays independent.
	h := resolveClineHeaders(pluginConfig{ClientVersion: "9.9.99"})
	if h.Version != "9.9.99" || h.PlatformVer != "9.9.99" {
		t.Fatalf("explicit client_version: version=%q plat=%q", h.Version, h.PlatformVer)
	}
	if h.CoreVersion != defaultCoreVersion {
		t.Fatalf("core must not follow client_version, got %q", h.CoreVersion)
	}

	// 2. A cache newer than the fallback is used when config is empty.
	setVersionCacheForTest("desktop", "9.0.1")
	h2 := resolveClineHeaders(pluginConfig{ClientType: "cline-desktop"})
	if h2.Version != "9.0.1" || h2.PlatformVer != "9.0.1" {
		t.Fatalf("expected newer cached desktop 9.0.1, got %q/%q", h2.Version, h2.PlatformVer)
	}

	// 3. CLI profile resolves to the "cli" key.
	setVersionCacheForTest("cli", "9.1.99")
	h3 := resolveClineHeaders(pluginConfig{Profile: "cli"})
	if h3.Version != "9.1.99" {
		t.Fatalf("expected CLI dynamic version 9.1.99, got %q", h3.Version)
	}
	if h3.ClientType != "cline-cli" || h3.Platform != "cli" {
		t.Fatalf("expected cli profile types, got type=%q plat=%q", h3.ClientType, h3.Platform)
	}
}

func TestDiskSnapshotPersistenceAndColdStart(t *testing.T) {
	tempDir := t.TempDir()
	snapshotFile := filepath.Join(tempDir, "test-version-cache.json")
	customCachePath = snapshotFile
	defer func() { customCachePath = testDefaultCachePath }()

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
	resetVersionCacheForTest(t)
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
	if req.Header.Get("X-Core-Version") != defaultCoreVersion {
		t.Fatalf("X-Core-Version must be the @cline/core version, got %s", req.Header.Get("X-Core-Version"))
	}
}

func overrideVersionEndpoints(t *testing.T, githubURL, npmBase string) {
	t.Helper()
	versionMu.Lock()
	prevGH, prevNPM := githubReleasesURL, npmRegistryBaseURL
	githubReleasesURL = githubURL
	npmRegistryBaseURL = npmBase
	versionMu.Unlock()
	t.Cleanup(func() {
		versionMu.Lock()
		githubReleasesURL = prevGH
		npmRegistryBaseURL = prevNPM
		versionMu.Unlock()
	})
}

func TestFetchLatestDesktopReleaseVersion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") == "" {
			t.Error("missing Accept header")
		}
		_, _ = w.Write([]byte(`[
			{"tag_name":"v1.0.0"},
			{"tag_name":"desktop-v"},
			{"tag_name":"desktop-v3.1.4"}
		]`))
	}))
	defer srv.Close()
	overrideVersionEndpoints(t, srv.URL, srv.URL)

	ver, err := fetchLatestDesktopReleaseVersion()
	if err != nil {
		t.Fatal(err)
	}
	if ver != "3.1.4" {
		t.Fatalf("version=%q want 3.1.4", ver)
	}
}

func TestFetchLatestDesktopReleaseVersionErrors(t *testing.T) {
	t.Run("http error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
		}))
		defer srv.Close()
		overrideVersionEndpoints(t, srv.URL, srv.URL)
		if _, err := fetchLatestDesktopReleaseVersion(); err == nil {
			t.Fatal("expected github HTTP error")
		}
	})
	t.Run("no desktop tag", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`[{"tag_name":"cli-v9.9.9"}]`))
		}))
		defer srv.Close()
		overrideVersionEndpoints(t, srv.URL, srv.URL)
		if _, err := fetchLatestDesktopReleaseVersion(); err == nil {
			t.Fatal("expected missing desktop-v tag")
		}
	})
	t.Run("invalid json", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("oops"))
		}))
		defer srv.Close()
		overrideVersionEndpoints(t, srv.URL, srv.URL)
		if _, err := fetchLatestDesktopReleaseVersion(); err == nil {
			t.Fatal("expected unmarshal error")
		}
	})
}

func TestFetchLatestNpmVersion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/cline" && r.URL.Path != "/@cline/core" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"dist-tags":{"latest":"3.2.1"}}`))
	}))
	defer srv.Close()
	overrideVersionEndpoints(t, srv.URL, srv.URL)

	ver, err := fetchLatestNpmVersion("cline")
	if err != nil {
		t.Fatal(err)
	}
	if ver != "3.2.1" {
		t.Fatalf("npm version=%q", ver)
	}
}

func TestFetchLatestNpmVersionHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	overrideVersionEndpoints(t, srv.URL, srv.URL)
	if _, err := fetchLatestNpmVersion("missing"); err == nil {
		t.Fatal("expected npm HTTP error")
	}
}

func TestRefreshVersionInBackgroundDesktopAndCLI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.RawQuery == "per_page=10" || r.URL.Path == "/" || r.URL.Path == "":
			// GitHub releases (full URL override points at this server root)
			if r.Header.Get("Accept") == "application/vnd.github.v3+json" {
				_, _ = w.Write([]byte(`[{"tag_name":"desktop-v4.0.1"}]`))
				return
			}
			fallthrough
		case r.URL.Path == "/cline":
			_, _ = w.Write([]byte(`{"dist-tags":{"latest":"5.0.2"}}`))
		case r.URL.Path == "/@cline/core":
			_, _ = w.Write([]byte(`{"dist-tags":{"latest":"4.0.9"}}`))
		default:
			_, _ = w.Write([]byte(`[{"tag_name":"desktop-v4.0.1"}]`))
		}
	}))
	defer srv.Close()
	overrideVersionEndpoints(t, srv.URL, srv.URL)

	snapshot := filepath.Join(t.TempDir(), "cline-version-cache.json")
	customCachePath = snapshot
	t.Cleanup(func() { customCachePath = testDefaultCachePath })

	versionMu.Lock()
	versionCache = map[string]versionCacheEntry{}
	versionMu.Unlock()

	refreshVersionInBackground(targetKeyDesktop)
	refreshVersionInBackground(targetKeyCLI)

	versionMu.RLock()
	desk := versionCache[targetKeyDesktop].Version
	cli := versionCache[targetKeyCLI].Version
	versionMu.RUnlock()
	if desk != "4.0.1" {
		t.Fatalf("desktop cache=%q want 4.0.1", desk)
	}
	if cli != "5.0.2" {
		t.Fatalf("cli cache=%q want 5.0.2", cli)
	}
	if _, err := os.Stat(snapshot); err != nil {
		t.Fatalf("snapshot not written: %v", err)
	}

	// Stampede guard: a second call within 10 minutes must not change FetchedAt's generation path.
	before := time.Now()
	refreshVersionInBackground(targetKeyDesktop)
	versionMu.RLock()
	after := versionCache[targetKeyDesktop].FetchedAt
	versionMu.RUnlock()
	if after.Before(before.Add(-time.Second)) {
		t.Fatalf("stampede guard lost FetchedAt: %v", after)
	}
}

// Desktop has no npm fallback: @cline/core is a different version line, so a
// GitHub failure must leave the desktop cache untouched (fallback keeps serving)
// even when npm would answer.
func TestRefreshVersionDesktopHasNoNpmFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") == "application/vnd.github.v3+json" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if r.URL.Path == "/@cline/core" {
			_, _ = w.Write([]byte(`{"dist-tags":{"latest":"6.6.6"}}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	overrideVersionEndpoints(t, srv.URL, srv.URL)
	resetVersionCacheForTest(t)

	refreshVersionInBackground(targetKeyDesktop)

	versionMu.RLock()
	got := versionCache[targetKeyDesktop].Version
	versionMu.RUnlock()
	if got != "" {
		t.Fatalf("desktop must not take the @cline/core version, got %q", got)
	}
	if v := resolveLiveVersion(targetKeyDesktop, defaultClientVersion); v != defaultClientVersion {
		t.Fatalf("fallback should serve, got %q", v)
	}

	// The core key does take the npm @cline/core version.
	refreshVersionInBackground(targetKeyCore)
	versionMu.RLock()
	core := versionCache[targetKeyCore].Version
	versionMu.RUnlock()
	if core != "6.6.6" {
		t.Fatalf("core cache=%q want 6.6.6", core)
	}
}

func TestLoadVersionSnapshotSkipsGarbage(t *testing.T) {
	customCachePath = filepath.Join(t.TempDir(), "bad.json")
	t.Cleanup(func() { customCachePath = testDefaultCachePath })
	if err := os.WriteFile(customCachePath, []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	versionMu.Lock()
	versionCache = map[string]versionCacheEntry{}
	versionMu.Unlock()
	loadVersionSnapshotFromDisk()
	versionMu.RLock()
	n := len(versionCache)
	versionMu.RUnlock()
	if n != 0 {
		t.Fatalf("garbage snapshot should not populate cache, got %d", n)
	}

	raw, _ := json.Marshal(map[string]versionCacheEntry{
		"desktop": {Version: "nope", FetchedAt: time.Now()},
		"cli":     {Version: "1.2.3", FetchedAt: time.Now()},
	})
	if err := os.WriteFile(customCachePath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	loadVersionSnapshotFromDisk()
	versionMu.RLock()
	_, bad := versionCache["desktop"]
	cli := versionCache["cli"].Version
	versionMu.RUnlock()
	if bad {
		t.Fatal("implausible desktop version should be skipped")
	}
	if cli != "1.2.3" {
		t.Fatalf("cli=%q", cli)
	}
}

func TestResolveLiveClientVersionFallbackAndStale(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	overrideVersionEndpoints(t, srv.URL, srv.URL)
	customCachePath = filepath.Join(t.TempDir(), "empty.json")
	t.Cleanup(func() { customCachePath = testDefaultCachePath })

	versionMu.Lock()
	delete(versionCache, targetKeyCLI)
	versionMu.Unlock()
	if got := resolveLiveVersion(targetKeyCLI, "0.0.1"); got != "0.0.1" {
		t.Fatalf("empty cache fallback=%q", got)
	}

	versionMu.Lock()
	versionCache[targetKeyDesktop] = versionCacheEntry{
		Version:   "2.2.2",
		FetchedAt: time.Now().Add(-7 * time.Hour),
	}
	versionMu.Unlock()
	if got := resolveLiveVersion(targetKeyDesktop, "0.0.1"); got != "2.2.2" {
		t.Fatalf("stale cache should still return last version, got %q", got)
	}
}

func TestStartVersionUpdater(t *testing.T) {
	// Warm-up is fire-and-forget; this test only pins that the exported
	// entry point is safe to call more than once (sync.Once). Fetch behaviour
	// is covered by TestRefreshVersionInBackgroundDesktopAndCLI.
	StartVersionUpdater()
	StartVersionUpdater()
}
