package plugin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Regression tests for the forced-refresh path (0.2.9).
//
// Background: a forced refresh runs after the upstream rejected our bearer. In
// 0.2.7 it compared four credential fields against the on-disk copy and skipped
// the exchange on *any* difference, so a benign difference (a stale refresh
// token, a re-serialised expiry) silently handed back the same dead bearer — the
// 401 then looked like a dead login. These tests pin the corrected rule: only a
// *different and still usable* on-disk bearer may make the exchange redundant.

// refreshServer counts exchanges and answers like the real endpoint.
func refreshServer(t *testing.T, calls *int32, status int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(calls, 1)
		w.Header().Set("Content-Type", "application/json")
		if status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"server_error","error_description":"boom"}`))
			return
		}
		body, _ := json.Marshal(map[string]any{
			"success": true,
			"data": map[string]any{
				"accessToken": "NEW-ACCESS", "refreshToken": "NEW-REFRESH",
				"expiresAt": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
				"tokenType": "Bearer",
				"userInfo":  map[string]any{"email": "force@example.com", "clineUserId": "usr-1"},
			},
		})
		_, _ = w.Write(body)
	}))
}

func resetThrottledLogs() {
	throttledLogMu.Lock()
	throttledLogSeen = map[string]time.Time{}
	throttledLogMu.Unlock()
}

// writeAuthFile stores one credential document for the given email.
func writeAuthFile(t *testing.T, dir, email string, doc map[string]any) {
	t.Helper()
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, clineAuthFileName(email)), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readPluginLog(t *testing.T, cfg pluginConfig) string {
	t.Helper()
	raw, err := os.ReadFile(pluginLogPath(cfg))
	if err != nil {
		return ""
	}
	return string(raw)
}

// A disk copy that differs only in a non-bearer field must NOT suppress the
// exchange: this is the exact 0.2.7 defect that made a recoverable 401 fatal.
func TestForceRefreshExchangesWhenDiskDiffersInNonTokenFields(t *testing.T) {
	var calls int32
	srv := refreshServer(t, &calls, http.StatusOK)
	defer srv.Close()

	dir := t.TempDir()
	cfg := pluginConfig{AuthDir: dir, BaseURL: srv.URL}
	email := "force@example.com"
	// Disk holds the same (rejected) bearer but a stale refresh token.
	writeAuthFile(t, dir, email, map[string]any{
		"type": "cline", "access_token": "workos:DEAD", "refresh_token": "rt-disk",
		"expires_at": time.Now().Add(-2 * time.Minute).UnixMilli(), "email": email, "account_id": "usr-1",
	})
	snapshot := &clineOAuthStorage{
		Type: ProviderKey, AccessToken: "workos:DEAD", RefreshToken: "rt-host",
		ExpiresAt: time.Now().Add(-time.Minute).UnixMilli(), Email: email, AccountID: "usr-1",
	}
	cred := credential{bearer: "workos:DEAD", source: "oauth", oauth: true, storage: snapshot}

	out, changed := forceRefreshCredential(cfg, cred)
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("upstream exchanges = %d, want 1 (a forced refresh must really exchange)", got)
	}
	if !changed {
		t.Fatal("changed = false, want true after a successful exchange")
	}
	if out.bearer == "workos:DEAD" {
		t.Fatalf("bearer was not replaced: %q", out.bearer)
	}
}

// The one legitimate skip: disk already has a *different and usable* bearer, so a
// second exchange would be a duplicate rotation.
func TestForceRefreshAdoptsNewerUsableDiskCredential(t *testing.T) {
	var calls int32
	srv := refreshServer(t, &calls, http.StatusOK)
	defer srv.Close()

	dir := t.TempDir()
	cfg := pluginConfig{AuthDir: dir, BaseURL: srv.URL}
	email := "force@example.com"
	writeAuthFile(t, dir, email, map[string]any{
		"type": "cline", "access_token": "workos:FRESH-FROM-CLI", "refresh_token": "rt-disk",
		"expires_at": time.Now().Add(50 * time.Minute).UnixMilli(), "email": email, "account_id": "usr-1",
	})
	snapshot := &clineOAuthStorage{
		Type: ProviderKey, AccessToken: "workos:DEAD", RefreshToken: "rt-host",
		ExpiresAt: time.Now().Add(-time.Minute).UnixMilli(), Email: email, AccountID: "usr-1",
	}
	cred := credential{bearer: "workos:DEAD", source: "oauth", oauth: true, storage: snapshot}

	out, changed := forceRefreshCredential(cfg, cred)
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("upstream exchanges = %d, want 0 (adopting an already rotated credential)", got)
	}
	if !changed || out.bearer != "workos:FRESH-FROM-CLI" {
		t.Fatalf("adopted bearer = %q (changed=%v), want the disk bearer", out.bearer, changed)
	}
	if log := readPluginLog(t, cfg); !strings.Contains(log, "adopting the newer usable credential") {
		t.Fatalf("adoption decision was not logged:\n%s", log)
	}
}

// A different but stale disk bearer is no replacement: exchange anyway.
func TestForceRefreshExchangesWhenDiskBearerIsStale(t *testing.T) {
	var calls int32
	srv := refreshServer(t, &calls, http.StatusOK)
	defer srv.Close()

	dir := t.TempDir()
	cfg := pluginConfig{AuthDir: dir, BaseURL: srv.URL}
	email := "force@example.com"
	writeAuthFile(t, dir, email, map[string]any{
		"type": "cline", "access_token": "workos:ALSO-DEAD", "refresh_token": "rt-disk",
		"expires_at": time.Now().Add(-30 * time.Second).UnixMilli(), "email": email, "account_id": "usr-1",
	})
	snapshot := &clineOAuthStorage{
		Type: ProviderKey, AccessToken: "workos:DEAD", RefreshToken: "rt-host",
		ExpiresAt: time.Now().Add(-time.Minute).UnixMilli(), Email: email, AccountID: "usr-1",
	}
	cred := credential{bearer: "workos:DEAD", source: "oauth", oauth: true, storage: snapshot}

	if _, changed := forceRefreshCredential(cfg, cred); !changed {
		t.Fatal("changed = false, want true: a stale disk bearer must not suppress the exchange")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("upstream exchanges = %d, want 1", got)
	}
}

// Every "we did not retry" decision must be on record — this silence is what made
// the 2026-09-23 outage undiagnosable from the plugin side.
func TestForceRefreshWithoutNewBearerIsLogged(t *testing.T) {
	var calls int32
	srv := refreshServer(t, &calls, http.StatusInternalServerError)
	defer srv.Close()

	dir := t.TempDir()
	cfg := pluginConfig{AuthDir: dir, BaseURL: srv.URL}
	email := "force@example.com"
	writeAuthFile(t, dir, email, map[string]any{
		"type": "cline", "access_token": "workos:DEAD", "refresh_token": "rt-disk",
		"expires_at": time.Now().Add(-2 * time.Minute).UnixMilli(), "email": email, "account_id": "usr-1",
	})
	snapshot := &clineOAuthStorage{
		Type: ProviderKey, AccessToken: "workos:DEAD", RefreshToken: "rt-disk",
		ExpiresAt: time.Now().Add(-2 * time.Minute).UnixMilli(), Email: email, AccountID: "usr-1",
	}
	cred := credential{bearer: "workos:DEAD", source: "oauth", oauth: true, storage: snapshot}

	if _, changed := forceRefreshCredential(cfg, cred); changed {
		t.Fatal("changed = true, want false when the exchange fails")
	}
	log := readPluginLog(t, cfg)
	if !strings.Contains(log, "forced refresh produced no credential") {
		t.Fatalf("failed forced refresh left no trace:\n%s", log)
	}
	if !strings.Contains(log, "refresh transient failure") {
		t.Fatalf("inner refresh failure left no trace:\n%s", log)
	}
}

// A stale bearer that cannot be refreshed at all is warned about (throttled),
// instead of silently travelling upstream.
func TestStaleBearerWithoutRefreshTokenIsWarned(t *testing.T) {
	dir := t.TempDir()
	cfg := pluginConfig{AuthDir: dir, BaseURL: "http://127.0.0.1:1/api/v1"}
	resetThrottledLogs()
	t.Cleanup(resetThrottledLogs)

	req := executorRequest{Metadata: map[string]any{
		"access_token": "workos:DEAD",
		"expires_at":   float64(time.Now().Add(-time.Hour).UnixMilli()),
		"email":        "stale@example.com",
	}}
	cred, err := resolveCredentials(cfg, req)
	if err != nil {
		t.Fatalf("resolveCredentials: %v", err)
	}
	if cred.source != "oauth" {
		t.Fatalf("source = %q, want oauth (the stale bearer is still surfaced)", cred.source)
	}
	_, _ = resolveCredentials(cfg, req) // second call must be suppressed by the throttle

	log := readPluginLog(t, cfg)
	if got := strings.Count(log, "no refresh_token and its access token is stale"); got != 1 {
		t.Fatalf("expected exactly 1 stale-bearer warning, got %d:\n%s", got, log)
	}
}
