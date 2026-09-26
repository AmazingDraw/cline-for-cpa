package plugin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests pin the 0.2.8 "host refresh contract": the plugin must (a) tell the
// host when to poll it for a refresh, and (b) never report a recoverable
// credential failure as 401, because the host treats any 401 from a plugin as
// "unauthorized" and then stops refreshing that auth forever
// (CLIProxyAPI sdk/cliproxy/auth: isUnauthorizedError / applyAuthFailureState /
// hasUnauthorizedAuthFailure → 503 auth_unavailable that only a re-login clears).

type testEnvelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"http_status"`
	Retryable  *bool  `json:"retryable"`
}

type testEnvelope struct {
	OK     bool               `json:"ok"`
	Result json.RawMessage    `json:"result"`
	Error  *testEnvelopeError `json:"error"`
}

// applyTestConfig applies a plugin config for the duration of one test and
// restores the built-in defaults afterwards, so global config state cannot leak
// between tests.
func applyTestConfig(t *testing.T, yaml string) {
	t.Helper()
	if err := applyConfig([]byte(yaml)); err != nil {
		t.Fatalf("applyConfig(%q): %v", yaml, err)
	}
	t.Cleanup(func() {
		if err := applyConfig(nil); err != nil {
			t.Fatalf("restore default config: %v", err)
		}
	})
}

func decodeEnvelope(t *testing.T, raw []byte) testEnvelope {
	t.Helper()
	var env testEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode envelope: %v (raw=%s)", err, raw)
	}
	return env
}

// parseAuthForTest drives the auth.parse ABI method with a credential document
// and returns the parsed Auth payload.
func parseAuthForTest(t *testing.T, storage map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(storage)
	if err != nil {
		t.Fatal(err)
	}
	req, err := json.Marshal(map[string]any{"raw_json": string(raw)})
	if err != nil {
		t.Fatal(err)
	}
	out, err := HandleMethod("auth.parse", req)
	if err != nil {
		t.Fatalf("auth.parse: %v", err)
	}
	env := decodeEnvelope(t, out)
	if !env.OK {
		t.Fatalf("auth.parse failed: %+v", env.Error)
	}
	var result struct {
		Handled bool           `json:"handled"`
		Auth    map[string]any `json:"Auth"`
	}
	if err := json.Unmarshal(env.Result, &result); err != nil {
		t.Fatalf("decode auth.parse result: %v", err)
	}
	if !result.Handled {
		t.Fatalf("auth.parse did not handle the document: %s", env.Result)
	}
	return result.Auth
}

// refreshAuthForTest drives the auth.refresh ABI method and returns the raw
// envelope (success or failure).
func refreshAuthForTest(t *testing.T, storage map[string]any) testEnvelope {
	t.Helper()
	raw, err := json.Marshal(storage)
	if err != nil {
		t.Fatal(err)
	}
	// storage_json is accepted as a plain JSON string by the loose probe path.
	req, err := json.Marshal(map[string]any{"storage_json": string(raw)})
	if err != nil {
		t.Fatal(err)
	}
	out, err := HandleMethod("auth.refresh", req)
	if err != nil {
		t.Fatalf("auth.refresh: %v", err)
	}
	return decodeEnvelope(t, out)
}

func clineStorageForTest(expiresIn time.Duration) map[string]any {
	return map[string]any{
		"type":          "cline",
		"access_token":  "workos:access-token",
		"refresh_token": "refresh-token",
		"expires_at":    time.Now().Add(expiresIn).UnixMilli(),
		"email":         "contract@example.com",
		"account_id":    "usr-contract",
	}
}

// --- (a) the host must be told when to poll us ---------------------------------

func TestAuthParsePublishesHostRefreshContract(t *testing.T) {
	applyTestConfig(t, "{}")
	auth := parseAuthForTest(t, clineStorageForTest(time.Hour))

	meta, _ := auth["Metadata"].(map[string]any)
	if meta == nil {
		t.Fatal("auth.parse returned no Metadata")
	}
	got, ok := meta["refresh_interval_seconds"]
	if !ok {
		t.Fatal("Metadata is missing refresh_interval_seconds — the host would never refresh this auth proactively")
	}
	seconds, isNumber := got.(float64) // JSON numbers decode as float64, which the host accepts
	if !isNumber || int(seconds) != defaultHostRefreshIntervalSeconds {
		t.Fatalf("refresh_interval_seconds = %#v, want a number %d", got, defaultHostRefreshIntervalSeconds)
	}

	rawNext, _ := auth["NextRefreshAfter"].(string)
	if rawNext == "" {
		t.Fatal("auth.parse returned no NextRefreshAfter (PascalCase is required by the untagged host struct)")
	}
	next, err := time.Parse(time.RFC3339, rawNext)
	if err != nil {
		t.Fatalf("NextRefreshAfter is not RFC3339: %q", rawNext)
	}
	// The wake point must land one published interval before expiry. The fixture's
	// token lifetime is one hour, so the wake point sits (lifetime − interval) in
	// the future; deriving it from the constant keeps the test tracking the
	// default instead of pinning a magic number.
	want := time.Hour - time.Duration(defaultHostRefreshIntervalSeconds)*time.Second
	if remaining := time.Until(next); remaining < want-time.Minute || remaining > want+time.Minute {
		t.Fatalf("NextRefreshAfter is %v away, want ≈%v (expiry − refresh_interval_seconds)", remaining, want)
	}
}

func TestAuthParseContractCanBeDisabled(t *testing.T) {
	applyTestConfig(t, `{"refresh_interval_seconds": 0}`)
	auth := parseAuthForTest(t, clineStorageForTest(time.Hour))

	meta, _ := auth["Metadata"].(map[string]any)
	if _, present := meta["refresh_interval_seconds"]; present {
		t.Fatal("refresh_interval_seconds must be omitted when the contract is disabled")
	}
	rawNext, _ := auth["NextRefreshAfter"].(string)
	if rawNext == "" {
		t.Fatal("expected the legacy 5-minute NextRefreshAfter when the contract is disabled")
	}
	next, err := time.Parse(time.RFC3339, rawNext)
	if err != nil {
		t.Fatal(err)
	}
	if remaining := time.Until(next); remaining < 50*time.Minute {
		t.Fatalf("legacy wake point should sit ≈5m before expiry, got %v", remaining)
	}
}

func TestHostWakeAtIsNeverInThePast(t *testing.T) {
	cfg := pluginConfig{HostRefreshIntervalSeconds: intPtr(defaultHostRefreshIntervalSeconds)}
	// Expiry already inside the window (or past): the floor stops the host from
	// re-polling on every loop.
	for _, expiresIn := range []time.Duration{0, -time.Minute, time.Minute} {
		wake := hostWakeAt(cfg, &clineOAuthStorage{ExpiresAt: time.Now().Add(expiresIn).UnixMilli()})
		if !wake.After(time.Now()) {
			t.Fatalf("expiresIn=%v: wake %v is not in the future", expiresIn, wake)
		}
	}
	// No expiry → no opinion.
	if zero := hostWakeAt(cfg, &clineOAuthStorage{}); !zero.IsZero() {
		t.Fatalf("unknown expiry must yield a zero wake point, got %v", zero)
	}
}

// --- (b) a recoverable failure must never look like a dead login ---------------

func TestClassify401AfterRefresh(t *testing.T) {
	// Not recovered: the login is fine, we just could not mint a token right now.
	transient := ClassifyUpstreamHTTPForSourceAfterRefresh(http.StatusUnauthorized, "unauthorized", "oauth", false)
	if transient.status != http.StatusServiceUnavailable {
		t.Fatalf("unrecovered oauth 401 status = %d, want 503", transient.status)
	}
	if transient.code != "oauth_refresh_unavailable" {
		t.Fatalf("code = %q, want oauth_refresh_unavailable", transient.code)
	}
	if transient.retryable == nil || !*transient.retryable {
		t.Fatal("an unrecovered oauth 401 must be retryable")
	}

	// Recovered: a brand-new bearer was rejected too → the subscription really is dead.
	recovered := ClassifyUpstreamHTTPForSourceAfterRefresh(http.StatusUnauthorized, "unauthorized", "oauth", true)
	if recovered.status != http.StatusUnauthorized || recovered.code != "cline_reauth_required" {
		t.Fatalf("recovered oauth 401 = %d/%q, want 401/cline_reauth_required", recovered.status, recovered.code)
	}
	if recovered.retryable == nil || *recovered.retryable {
		t.Fatal("a rejected fresh token must not be retryable")
	}

	// API-key credentials keep their own (accurate) classification.
	key := ClassifyUpstreamHTTPForSourceAfterRefresh(http.StatusUnauthorized, "", "api_key", false)
	if key.status != http.StatusUnauthorized || key.code != "invalid_api_key" {
		t.Fatalf("api_key 401 = %d/%q", key.status, key.code)
	}

	// The legacy entry point keeps the "already refreshed" semantics its callers rely on.
	if legacy := ClassifyUpstreamHTTPForSource(http.StatusUnauthorized, "unauthorized", "oauth"); legacy.code != "cline_reauth_required" {
		t.Fatalf("legacy wrapper changed behaviour: %q", legacy.code)
	}
}

// TestAuthRefreshTransientFailureIsRetryable503 forces a refresh that cannot
// reach the upstream at all (connection refused) while the cached token is
// already expired: the correct answer is 503 + retryable, NOT 401.
func TestAuthRefreshTransientFailureIsRetryable503(t *testing.T) {
	applyTestConfig(t, `{"auth_dir": "`+t.TempDir()+`", "base_url": "http://127.0.0.1:1/api/v1"}`)
	env := refreshAuthForTest(t, clineStorageForTest(-time.Minute))

	if env.OK || env.Error == nil {
		t.Fatalf("expected a failure envelope, got %s", env.Result)
	}
	if env.Error.HTTPStatus != http.StatusServiceUnavailable {
		t.Fatalf("http_status = %d, want 503 (a 401 here would disable the host auth forever)", env.Error.HTTPStatus)
	}
	if env.Error.Code != "oauth_refresh_unavailable" {
		t.Fatalf("code = %q, want oauth_refresh_unavailable", env.Error.Code)
	}
	if env.Error.Retryable == nil || !*env.Error.Retryable {
		t.Fatal("transient refresh failure must advertise retryable=true")
	}
}

// TestAuthRefreshRejectedTokenRequiresReauth is the mirror image: when the
// upstream genuinely rejects the refresh token, 401 + cline_reauth_required is
// the honest answer and the user must sign in again.
func TestAuthRefreshRejectedTokenRequiresReauth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"Bad Request"}`))
	}))
	defer server.Close()

	applyTestConfig(t, `{"auth_dir": "`+t.TempDir()+`", "base_url": "`+server.URL+`"}`)
	env := refreshAuthForTest(t, clineStorageForTest(-time.Minute))

	if env.OK || env.Error == nil {
		t.Fatalf("expected a failure envelope, got %s", env.Result)
	}
	if env.Error.HTTPStatus != http.StatusUnauthorized {
		t.Fatalf("http_status = %d, want 401", env.Error.HTTPStatus)
	}
	if env.Error.Code != "cline_reauth_required" {
		t.Fatalf("code = %q, want cline_reauth_required", env.Error.Code)
	}
	if env.Error.Retryable == nil || *env.Error.Retryable {
		t.Fatal("a rejected refresh token must advertise retryable=false")
	}
}

// --- credential recovery --------------------------------------------------------

func TestStorageFallbackReadsExpiryAndAccount(t *testing.T) {
	expiry := time.Now().Add(42 * time.Minute).UnixMilli()
	st := parseStorageFromRequest(nil, map[string]any{
		"access_token":  "a",
		"refresh_token": "r",
		"expires_at":    float64(expiry), // JSON numbers arrive as float64
		"account_id":    "usr-9",
	}, nil)
	if st == nil {
		t.Fatal("metadata-only credential was rejected")
	}
	if st.ExpiresAt != expiry {
		t.Fatalf("ExpiresAt = %d, want %d", st.ExpiresAt, expiry)
	}
	if st.AccountID != "usr-9" {
		t.Fatalf("AccountID = %q", st.AccountID)
	}
}

func TestWithDiskFallbackCompletesTrimmedCredential(t *testing.T) {
	dir := t.TempDir()
	email := "fallback@example.com"
	onDisk := map[string]any{
		"type": "cline", "access_token": "disk-access", "refresh_token": "disk-refresh",
		"expires_at": time.Now().Add(30 * time.Minute).UnixMilli(), "email": email,
	}
	raw, _ := json.Marshal(onDisk)
	if err := os.WriteFile(filepath.Join(dir, clineAuthFileName(email)), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := pluginConfig{AuthDir: dir}

	trimmed := &clineOAuthStorage{Type: ProviderKey, AccessToken: "host-access", Email: email}
	repaired := withDiskFallback(cfg, trimmed)
	if repaired.RefreshToken != "disk-refresh" {
		t.Fatalf("refresh token not recovered from disk: %+v", repaired)
	}
	if repaired.ExpiresAt == 0 {
		t.Fatal("expiry not recovered from disk")
	}
	if repaired.AccessToken != "host-access" {
		t.Fatalf("host-supplied access token was overwritten: %q", repaired.AccessToken)
	}
}

func TestWithDiskFallbackRefusesAmbiguity(t *testing.T) {
	dir := t.TempDir()
	for _, email := range []string{"a@example.com", "b@example.com"} {
		raw, _ := json.Marshal(map[string]any{
			"type": "cline", "refresh_token": "rt-" + email,
			"expires_at": time.Now().Add(time.Hour).UnixMilli(), "email": email,
		})
		if err := os.WriteFile(filepath.Join(dir, clineAuthFileName(email)), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := pluginConfig{AuthDir: dir}
	// No email to locate the file plus two candidates on disk: guessing would
	// spend the wrong subscription, so the credential stays exactly as supplied.
	got := withDiskFallback(cfg, &clineOAuthStorage{Type: ProviderKey, AccessToken: "host-access"})
	if got.RefreshToken != "" {
		t.Fatalf("ambiguous fallback must be refused, got refresh token %q", got.RefreshToken)
	}
}

// --- observability of the contract ----------------------------------------------

// TestHostPollLogIsThrottled pins the signal operations relies on: a no-op host
// poll is logged at most once per credential per hour, so a log line *before* the
// token approaches expiry proves the host honoured refresh_interval_seconds.
func TestHostPollLogIsThrottled(t *testing.T) {
	dir := t.TempDir()
	cfg := pluginConfig{AuthDir: filepath.Join(dir, "auths")}
	st := &clineOAuthStorage{
		Type: ProviderKey, AccessToken: "a", RefreshToken: "r",
		ExpiresAt: time.Now().Add(50 * time.Minute).UnixMilli(),
		Email:     "poll@example.com",
	}
	resetThrottledLogs()
	t.Cleanup(resetThrottledLogs)

	logHostPollWithoutRefresh(cfg, st)
	logHostPollWithoutRefresh(cfg, st) // must be suppressed by the throttle

	raw, err := os.ReadFile(pluginLogPath(cfg))
	if err != nil {
		t.Fatalf("expected a plugin log at %s: %v", pluginLogPath(cfg), err)
	}
	if got := strings.Count(string(raw), "host polled auth.refresh"); got != 1 {
		t.Fatalf("expected exactly 1 poll log line, got %d:\n%s", got, raw)
	}
}
