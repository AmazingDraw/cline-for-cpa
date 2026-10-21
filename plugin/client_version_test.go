package plugin

import (
	"net/http"
	"testing"
	"time"
)

func resetVersionCacheForTest(t *testing.T) {
	t.Helper()
	versionMu.Lock()
	prev := versionCache
	versionCache = map[string]versionCacheEntry{}
	versionMu.Unlock()
	t.Cleanup(func() {
		versionMu.Lock()
		versionCache = prev
		versionMu.Unlock()
	})
}

func setVersionCacheForTest(key, v string) {
	versionMu.Lock()
	versionCache[key] = versionCacheEntry{Version: v, FetchedAt: time.Now()}
	versionMu.Unlock()
}

func headersFor(t *testing.T, cfg pluginConfig) http.Header {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, "https://example.invalid/x", nil)
	applyClineHeaders(req, cfg)
	return req.Header
}

func wantHeaders(t *testing.T, got http.Header, want map[string]string) {
	t.Helper()
	for k, v := range want {
		if g := got.Get(k); g != v {
			t.Fatalf("%s = %q, want %q", k, g, v)
		}
	}
}

// Desktop identity: client version and core version are independent sources,
// each resolved as max(cache, fallback).
func TestDesktopHeadersCacheVsFallback(t *testing.T) {
	cases := []struct {
		name               string
		cacheDesk, cacheCo string
		wantClient         string
		wantCore           string
	}{
		{"cache missing", "", "", defaultClientVersion, defaultCoreVersion},
		{"cache older than fallback", "0.0.1", "0.0.2", defaultClientVersion, defaultCoreVersion},
		{"cache newer than fallback", "9.0.48", "9.0.95", "9.0.48", "9.0.95"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetVersionCacheForTest(t)
			if tc.cacheDesk != "" {
				setVersionCacheForTest(targetKeyDesktop, tc.cacheDesk)
			}
			if tc.cacheCo != "" {
				setVersionCacheForTest(targetKeyCore, tc.cacheCo)
			}
			wantHeaders(t, headersFor(t, pluginConfig{}), map[string]string{
				"User-Agent":         "Cline/" + tc.wantClient,
				"X-Client-Type":      "cline-desktop",
				"X-Client-Version":   tc.wantClient,
				"X-Platform":         "Cline Desktop",
				"X-Platform-Version": tc.wantClient,
				"X-Core-Version":     tc.wantCore,
			})
		})
	}
}

func TestCLIHeadersCacheVsFallback(t *testing.T) {
	cases := []struct {
		name, cacheCLI, want string
	}{
		{"cache missing", "", defaultCLIClientVersion},
		{"cache older than fallback", "1.0.0", defaultCLIClientVersion},
		{"cache newer than fallback", "9.9.9", "9.9.9"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetVersionCacheForTest(t)
			if tc.cacheCLI != "" {
				setVersionCacheForTest(targetKeyCLI, tc.cacheCLI)
			}
			// A desktop cache entry must never leak into the CLI identity.
			setVersionCacheForTest(targetKeyDesktop, "8.8.8")
			wantHeaders(t, headersFor(t, pluginConfig{Profile: "cli"}), map[string]string{
				"User-Agent":         "Cline/" + tc.want,
				"X-Client-Type":      "cline-cli",
				"X-Client-Version":   tc.want,
				"X-Platform":         "cli",
				"X-Platform-Version": tc.want,
				"X-Core-Version":     defaultCoreVersion,
			})
		})
	}
}

// Explicit config beats the cache, and core_version is independent.
func TestExplicitVersionConfigWins(t *testing.T) {
	resetVersionCacheForTest(t)
	setVersionCacheForTest(targetKeyDesktop, "9.9.9")
	setVersionCacheForTest(targetKeyCore, "9.9.9")
	wantHeaders(t, headersFor(t, pluginConfig{ClientVersion: "Cline/0.0.40", CoreVersion: "0.0.90"}), map[string]string{
		"User-Agent":       "Cline/0.0.40",
		"X-Client-Version": "0.0.40",
		"X-Core-Version":   "0.0.90",
	})
}

// The default config must leave client_version empty so the live cache is used.
func TestDefaultConfigLeavesClientVersionUnset(t *testing.T) {
	if v := defaultConfig().ClientVersion; v != "" {
		t.Fatalf("defaultConfig().ClientVersion = %q; a prefilled value silently disables live versions", v)
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.0.48", "0.0.95", -1},
		{"0.0.95", "0.0.48", 1},
		{"3.0.73", "3.0.73", 0},
		{"0.1.0", "0.0.99", 1},
		{"1.2.3-alpha", "1.2.3", -1},
		{"1.2", "1.2.0", 0},
		{"v2.0.0", "1.9.9", 1},
	}
	for _, c := range cases {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Fatalf("compareVersions(%q,%q)=%d want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestPlatformFromClientType(t *testing.T) {
	for in, want := range map[string]string{
		"":              "Cline Desktop",
		"cline-desktop": "Cline Desktop",
		"cline-cli":     "cli",
		"cline-sdk":     "sdk",
		"custom":        "custom",
	} {
		if got := platformFromClientType(in); got != want {
			t.Fatalf("platformFromClientType(%q)=%q want %q", in, got, want)
		}
	}
}
