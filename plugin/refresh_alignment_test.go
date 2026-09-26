package plugin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// --- D1: 30s retryable grace (official DEFAULT_RETRYABLE_TOKEN_GRACE_MS) ---

func TestAuthStillUsableGraceWindow(t *testing.T) {
	cases := []struct {
		name    string
		left    time.Duration
		want    bool
		comment string
	}{
		{"expired", -time.Minute, false, "expired → unusable"},
		{"inside grace", 10 * time.Second, false, "official keeps only >30s"},
		{"at grace edge", 31 * time.Second, true, "just above grace"},
		{"plenty left", 30 * time.Minute, true, "fresh"},
	}
	for _, tc := range cases {
		st := &clineOAuthStorage{AccessToken: "a", ExpiresAt: time.Now().Add(tc.left).UnixMilli()}
		if got := authStillUsable(st); got != tc.want {
			t.Fatalf("%s: authStillUsable = %v, want %v (%s)", tc.name, got, tc.want, tc.comment)
		}
	}
	if authStillUsable(&clineOAuthStorage{ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}) {
		t.Fatal("no access token must be unusable")
	}
	if authStillUsable(nil) {
		t.Fatal("nil credential must be unusable")
	}
}

func TestTransientFailureNeedsGraceToKeepToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":"boom"}`)
	}))
	defer srv.Close()

	// 10s left + transient failure → below the official grace → do not keep.
	cfg := pluginConfig{BaseURL: srv.URL, AuthDir: t.TempDir()}
	nearExpiry := clineOAuthStorage{
		Type: ProviderKey, AccessToken: "almost-gone", RefreshToken: "rt",
		ExpiresAt: time.Now().Add(10 * time.Second).UnixMilli(),
	}
	raw, _ := json.Marshal(nearExpiry)
	if _, err := resolveCredentials(cfg, executorRequest{StorageJSON: raw}); err == nil {
		t.Fatal("expected failure: credential is inside the 30s grace window")
	}

	// Comfortably valid + transient failure → keep serving (oauth_stale).
	healthy := clineOAuthStorage{
		Type: ProviderKey, AccessToken: "still-usable", RefreshToken: "rt",
		ExpiresAt: time.Now().Add(90 * time.Second).UnixMilli(),
	}
	raw2, _ := json.Marshal(healthy)
	got, err := resolveCredentials(cfg, executorRequest{StorageJSON: raw2})
	if err != nil {
		t.Fatalf("resolveCredentials: %v", err)
	}
	if got.source != "oauth_stale" {
		t.Fatalf("source = %q, want oauth_stale", got.source)
	}
}

// --- D2: four-field credential comparison (official authSettingsEqual) ---

func TestAuthSettingsEqualComparesFourFields(t *testing.T) {
	base := &clineOAuthStorage{
		AccessToken: "a", RefreshToken: "r", AccountID: "usr-1", ExpiresAt: 1000,
	}
	same := &clineOAuthStorage{
		AccessToken: "a", RefreshToken: "r", AccountID: "usr-1", ExpiresAt: 1000,
	}
	if !authSettingsEqual(base, same) {
		t.Fatal("identical credentials must compare equal")
	}
	mutations := map[string]func(*clineOAuthStorage){
		"access":    func(s *clineOAuthStorage) { s.AccessToken = "b" },
		"refresh":   func(s *clineOAuthStorage) { s.RefreshToken = "r2" },
		"accountId": func(s *clineOAuthStorage) { s.AccountID = "usr-2" },
		"expiresAt": func(s *clineOAuthStorage) { s.ExpiresAt = 2000 },
	}
	for name, mutate := range mutations {
		other := *base
		mutate(&other)
		if authSettingsEqual(base, &other) {
			t.Fatalf("differing %s must compare unequal (official compares all four)", name)
		}
	}
}

// --- D3: never resurrect a credential replaced mid-refresh ---

func TestRefreshDoesNotResurrectReplacedCredential(t *testing.T) {
	dir := t.TempDir()
	email := "resurrect@example.com"
	path := filepath.Join(dir, clineAuthFileName(email))

	baseline := clineOAuthStorage{
		Type: ProviderKey, AccessToken: "old-access", RefreshToken: "old-refresh",
		ExpiresAt: time.Now().Add(-time.Minute).UnixMilli(), Email: email, AccountID: "usr-1",
	}
	baseRaw, _ := json.Marshal(baseline)
	if err := os.WriteFile(path, baseRaw, 0o600); err != nil {
		t.Fatal(err)
	}

	// While our refresh is in flight, a sign-out/new sign-in replaces the file.
	replacement := clineOAuthStorage{
		Type: ProviderKey, AccessToken: "newer-access", RefreshToken: "newer-refresh",
		ExpiresAt: time.Now().Add(50 * time.Minute).UnixMilli(), Email: email, AccountID: "usr-2",
	}
	replacementRaw, _ := json.Marshal(replacement)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := os.WriteFile(path, replacementRaw, 0o600); err != nil {
			t.Errorf("rewrite during refresh: %v", err)
		}
		fmt.Fprintf(w, `{"success":true,"data":{"accessToken":"refreshed-access","refreshToken":"refreshed-refresh","tokenType":"Bearer","expiresAt":%q,"userInfo":{"email":%q,"clineUserId":"usr-1"}}}`,
			time.Now().Add(55*time.Minute).UTC().Format(time.RFC3339), email)
	}))
	defer srv.Close()

	cfg := pluginConfig{BaseURL: srv.URL, AuthDir: dir}
	if _, err := ensureFreshOAuth(cfg, &baseline); err == nil {
		t.Fatal("refresh must fail when the credential was replaced underneath it")
	}

	back, _ := os.ReadFile(path)
	var onDisk clineOAuthStorage
	if err := json.Unmarshal(back, &onDisk); err != nil {
		t.Fatal(err)
	}
	if onDisk.AccessToken != "newer-access" || onDisk.RefreshToken != "newer-refresh" {
		t.Fatalf("replaced credential was resurrected/overwritten: %+v", onDisk)
	}
}

func TestCredentialChangedOnDiskDetectsRemoval(t *testing.T) {
	dir := t.TempDir()
	email := "gone@example.com"
	path := filepath.Join(dir, clineAuthFileName(email))
	cfg := pluginConfig{AuthDir: dir}
	st := &clineOAuthStorage{Type: ProviderKey, Email: email, AccessToken: "a", RefreshToken: "r"}
	raw, _ := json.Marshal(st)
	_ = os.WriteFile(path, raw, 0o600)
	if credentialChangedOnDisk(cfg, st) {
		t.Fatal("unchanged credential must not report a change")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if !credentialChangedOnDisk(cfg, st) {
		t.Fatal("removed credential (sign-out) must report a change")
	}
}

// Official DEFAULT_HTTP_TIMEOUT_MS is 30s; the plugin's control-plane default
// must match it, and an explicit config value must still win.
func TestDefaultHTTPTimeoutMatchesOfficial(t *testing.T) {
	if got := httpTimeout(pluginConfig{}); got != 30*time.Second {
		t.Fatalf("default http timeout = %v, want official 30s", got)
	}
	if got := httpTimeout(pluginConfig{HTTPTimeoutSeconds: 45}); got != 45*time.Second {
		t.Fatalf("configured timeout = %v, want 45s", got)
	}
}
