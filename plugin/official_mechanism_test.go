package plugin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- header configurability (no rebuild on desktop version bump) ---

func TestApplyClineHeadersUsesDefaults(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://example.invalid/x", nil)
	cfg := pluginConfig{ClientVersion: defaultClientVersion}
	applyClineHeaders(req, cfg)
	want := map[string]string{
		"User-Agent":         "Cline/" + defaultClientVersion,
		"X-Client-Type":      defaultClientType,
		"X-Client-Version":   defaultClientVersion,
		"X-Platform":         "desktop", // derived from cline-desktop
		"X-Platform-Version": defaultClientVersion,
		"X-Core-Version":     defaultClientVersion,
		"X-IS-MULTIROOT":     "false",
		"HTTP-Referer":       defaultHTTPReferer,
		"X-Title":            defaultXTitle,
	}
	for k, v := range want {
		if got := req.Header.Get(k); got != v {
			t.Fatalf("%s = %q, want %q", k, got, v)
		}
	}
	if got := req.Header.Get("X-Task-ID"); got != "" {
		t.Fatalf("X-Task-ID = %q, want omitted by default", got)
	}
}

// The official builder derives platform from the cline-<source> convention and
// sends the full identity set; a half-populated set is what we must avoid.
func TestApplyClineHeadersProfileCLI(t *testing.T) {
	cfg := pluginConfig{Profile: "cli", ClientVersion: "3.0.64"}
	req, _ := http.NewRequest(http.MethodPost, "https://example.invalid/x", nil)
	applyClineHeaders(req, cfg)
	if got := req.Header.Get("X-Client-Type"); got != "cline-cli" {
		t.Fatalf("X-Client-Type = %q, want cline-cli", got)
	}
	if got := req.Header.Get("X-Platform"); got != "cli" {
		t.Fatalf("X-Platform = %q, want cli", got)
	}
	if got := req.Header.Get("User-Agent"); got != "Cline/3.0.64" {
		t.Fatalf("User-Agent = %q", got)
	}
}

func TestTaskIDSentWhenConfigured(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://example.invalid/x", nil)
	applyClineHeaders(req, pluginConfig{TaskID: "task_123"})
	if got := req.Header.Get("X-Task-ID"); got != "task_123" {
		t.Fatalf("X-Task-ID = %q", got)
	}
}

func TestApplyClineHeadersFollowsConfig(t *testing.T) {
	cfg := pluginConfig{ClientType: "cline-cli", ClientVersion: "Cline/9.9.9"}
	req, _ := http.NewRequest(http.MethodPost, "https://example.invalid/x", nil)
	applyClineHeaders(req, cfg)
	if got := req.Header.Get("User-Agent"); got != "Cline/9.9.9" {
		t.Fatalf("User-Agent = %q, want configured value (Cline/ prefix tolerated)", got)
	}
	if got := req.Header.Get("X-Client-Version"); got != "9.9.9" {
		t.Fatalf("X-Client-Version = %q, want bare version", got)
	}
	if got := req.Header.Get("x-client-type"); got != "cline-cli" {
		t.Fatalf("x-client-type = %q, want configured value", got)
	}
}

func TestApplyConfigKeepsHeaderOverrides(t *testing.T) {
	raw := []byte("client_version: Cline/1.2.3\nclient_type: cline-sdk\n")
	if err := applyConfig(raw); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = applyConfig(nil) })
	cfg := currentConfig()
	if cfg.ClientVersion != "Cline/1.2.3" || cfg.ClientType != "cline-sdk" {
		t.Fatalf("config not applied: %+v", cfg)
	}
}

// --- credential precedence: OAuth must not be hijacked by api_key ---

func TestOAuthBearerBeatsAPIKeyInSameAuthFile(t *testing.T) {
	cfg := pluginConfig{APIKey: "sk_should_not_win", BaseURL: "http://127.0.0.1:1", AuthDir: t.TempDir()}
	st := clineOAuthStorage{
		Type:         ProviderKey,
		AccessToken:  "fresh-access",
		RefreshToken: "live-refresh",
		ExpiresAt:    time.Now().Add(30 * time.Minute).UnixMilli(),
		APIKey:       "sk_same_file",
	}
	raw, _ := json.Marshal(st)
	got, err := resolveCredentials(cfg, executorRequest{StorageJSON: raw})
	if err != nil {
		t.Fatalf("resolveCredentials: %v", err)
	}
	if got.source != "oauth" {
		t.Fatalf("source = %q, want oauth (api_key must not hijack the subscription)", got.source)
	}
	if !strings.Contains(got.bearer, "fresh-access") {
		t.Fatalf("bearer = %q, want the OAuth access token", got.bearer)
	}
}

// --- failure semantics ---

func TestInvalidGrantReportsReauthRequired(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"invalid_grant","error_description":"refresh token revoked"}`)
	}))
	defer srv.Close()

	cfg := pluginConfig{BaseURL: srv.URL, AuthDir: t.TempDir()}
	st := clineOAuthStorage{
		Type:         ProviderKey,
		AccessToken:  "expired-access",
		RefreshToken: "revoked-refresh",
		ExpiresAt:    time.Now().Add(-time.Hour).UnixMilli(),
	}
	raw, _ := json.Marshal(st)
	_, err := resolveCredentials(cfg, executorRequest{StorageJSON: raw})
	if !isReauthRequired(err) {
		t.Fatalf("err = %v, want reauth-required", err)
	}
}

func TestTransientRefreshFailureKeepsValidToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":"boom"}`)
	}))
	defer srv.Close()

	cfg := pluginConfig{BaseURL: srv.URL, AuthDir: t.TempDir()}
	st := clineOAuthStorage{
		Type:         ProviderKey,
		AccessToken:  "still-usable",
		RefreshToken: "live-refresh",
		ExpiresAt:    time.Now().Add(90 * time.Second).UnixMilli(), // inside lead window, not expired
	}
	raw, _ := json.Marshal(st)
	got, err := resolveCredentials(cfg, executorRequest{StorageJSON: raw})
	if err != nil {
		t.Fatalf("resolveCredentials: %v", err)
	}
	if got.source != "oauth_stale" || !strings.Contains(got.bearer, "still-usable") {
		t.Fatalf("got %+v, want stale-but-valid OAuth token kept", got)
	}
}

// --- single-flight + refresh lock ---

func refreshTestServer(t *testing.T, calls *int32, delay time.Duration) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(calls, 1)
		time.Sleep(delay)
		expires := time.Now().Add(55 * time.Minute).UTC().Format(time.RFC3339)
		fmt.Fprintf(w, `{"success":true,"data":{"accessToken":"new-access","refreshToken":"new-refresh","tokenType":"Bearer","expiresAt":%q,"userInfo":{"email":"a@b.c","clineUserId":"usr-1"}}}`, expires)
	}))
}

func TestSingleFlightCoalescesConcurrentRefreshes(t *testing.T) {
	var calls int32
	srv := refreshTestServer(t, &calls, 150*time.Millisecond)
	defer srv.Close()

	cfg := pluginConfig{BaseURL: srv.URL, AuthDir: t.TempDir()}
	st := &clineOAuthStorage{
		Type:         ProviderKey,
		AccessToken:  "expired",
		RefreshToken: "live-refresh",
		ExpiresAt:    time.Now().Add(-time.Minute).UnixMilli(),
		Email:        "a@b.c",
	}
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := ensureFreshOAuth(cfg, st); err != nil {
				t.Errorf("ensureFreshOAuth: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("refresh calls = %d, want exactly 1 (single-flight)", got)
	}
}

func TestRefreshLockSerializesAcrossHolders(t *testing.T) {
	dir := t.TempDir()
	cfg := pluginConfig{AuthDir: dir}
	var (
		mu      sync.Mutex
		inside  int
		maxSeen int
	)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := withOAuthRefreshLock(cfg, func() error {
				mu.Lock()
				inside++
				if inside > maxSeen {
					maxSeen = inside
				}
				mu.Unlock()
				time.Sleep(40 * time.Millisecond)
				mu.Lock()
				inside--
				mu.Unlock()
				return nil
			}); err != nil {
				t.Errorf("withOAuthRefreshLock: %v", err)
			}
		}()
	}
	wg.Wait()
	if maxSeen != 1 {
		t.Fatalf("max concurrent holders = %d, want 1 (exclusive)", maxSeen)
	}
	if _, err := os.Stat(oauthLockPath(dir, diskProviderID, diskProviderID)); err != nil {
		t.Fatalf("lock file missing: %v", err)
	}
}

func TestLockFileNamesMatchOfficialScheme(t *testing.T) {
	path := oauthLockPath("/tmp/x", "providers.json", "cline")
	// Official: providers.json.oauth-<sha256("cline")>.lock
	want := "providers.json.oauth-84829dbd815311888f0e3d85822e9b07d14be89a480a3c09ee67353f0e806e3b.lock"
	if filepath.Base(path) != want {
		t.Fatalf("lock name = %q, want %q", filepath.Base(path), want)
	}
}

func TestRefreshReusesRotationFromDisk(t *testing.T) {
	var calls int32
	srv := refreshTestServer(t, &calls, 0)
	defer srv.Close()

	dir := t.TempDir()
	cfg := pluginConfig{BaseURL: srv.URL, AuthDir: dir}
	email := "rotate@example.com"
	// Another holder already rotated the credential on disk.
	onDisk := clineOAuthStorage{
		Type:         ProviderKey,
		AccessToken:  "rotated-access",
		RefreshToken: "rotated-refresh",
		ExpiresAt:    time.Now().Add(50 * time.Minute).UnixMilli(),
		Email:        email,
	}
	raw, _ := json.Marshal(onDisk)
	if err := os.WriteFile(filepath.Join(dir, clineAuthFileName(email)), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	stale := &clineOAuthStorage{
		Type:         ProviderKey,
		AccessToken:  "stale-access",
		RefreshToken: "stale-refresh",
		ExpiresAt:    time.Now().Add(-time.Minute).UnixMilli(),
		Email:        email,
	}
	fresh, err := ensureFreshOAuth(cfg, stale)
	if err != nil {
		t.Fatalf("ensureFreshOAuth: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("refresh calls = %d, want 0 (disk rotation reused under lock)", got)
	}
	if !strings.Contains(fresh.AccessToken, "rotated-access") {
		t.Fatalf("access = %q, want the rotated token from disk", fresh.AccessToken)
	}
}

// --- upstream 401 messaging ---

func TestUnauthorizedMessageDependsOnCredentialSource(t *testing.T) {
	oauthFailure := ClassifyUpstreamHTTPForSource(http.StatusUnauthorized, "unauthorized", "oauth")
	if !strings.Contains(oauthFailure.message, "重新授权") {
		t.Fatalf("oauth 401 message = %q, want re-auth guidance", oauthFailure.message)
	}
	if oauthFailure.code != "cline_reauth_required" {
		t.Fatalf("code = %q", oauthFailure.code)
	}
	keyFailure := ClassifyUpstreamHTTPForSource(http.StatusUnauthorized, "unauthorized", "api_key")
	if !strings.Contains(keyFailure.message, "API Key") {
		t.Fatalf("key 401 message = %q, want api-key guidance", keyFailure.message)
	}
}

func TestForceRefreshCredentialReportsChange(t *testing.T) {
	var calls int32
	srv := refreshTestServer(t, &calls, 0)
	defer srv.Close()

	cfg := pluginConfig{BaseURL: srv.URL, AuthDir: t.TempDir()}
	st := &clineOAuthStorage{
		Type: ProviderKey, AccessToken: "old", RefreshToken: "live", Email: "a@b.c",
		ExpiresAt: time.Now().Add(30 * time.Minute).UnixMilli(),
	}
	cred := credential{bearer: withWorkOSPrefix("old"), source: "oauth", oauth: true, storage: st}
	updated, changed := forceRefreshCredential(cfg, cred)
	if !changed {
		t.Fatal("forceRefreshCredential reported no change")
	}
	if !strings.Contains(updated.bearer, "new-access") {
		t.Fatalf("bearer = %q, want refreshed token", updated.bearer)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("refresh calls = %d, want 1", calls)
	}
}
