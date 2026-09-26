package plugin

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestWithWorkOSPrefix(t *testing.T) {
	if withWorkOSPrefix("abc") != "workos:abc" {
		t.Fatal("prefix")
	}
	if withWorkOSPrefix("workos:abc") != "workos:abc" {
		t.Fatal("idempotent")
	}
	if stripWorkOSPrefix("workos:abc") != "abc" {
		t.Fatal("strip")
	}
}

func TestResolveCredentialsFallsBackToPluginAPIKeyWhenRefreshFails(t *testing.T) {
	cfg := pluginConfig{APIKey: "sk_fallback_test_key", BaseURL: "http://127.0.0.1:1", AuthDir: t.TempDir()}
	st := clineOAuthStorage{
		Type:         ProviderKey,
		AccessToken:  "expired-access",
		RefreshToken: "dead-refresh",
		ExpiresAt:    time.Now().Add(-time.Hour).UnixMilli(),
	}
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	got, err := resolveCredentials(cfg, executorRequest{StorageJSON: raw})
	if err != nil {
		t.Fatalf("resolveCredentials error: %v", err)
	}
	if got.bearer != "sk_fallback_test_key" {
		t.Fatalf("got %q, want plugin api_key fallback", got.bearer)
	}
	if got.source != "api_key_fallback" {
		t.Fatalf("source = %q, want api_key_fallback", got.source)
	}
}

func TestResolveCredentialsKeepsValidAccessWithoutRefresh(t *testing.T) {
	cfg := pluginConfig{APIKey: "sk_should_not_use", BaseURL: "http://127.0.0.1:1", AuthDir: t.TempDir()}
	st := clineOAuthStorage{
		Type:         ProviderKey,
		AccessToken:  "still-good",
		RefreshToken: "unused-refresh",
		ExpiresAt:    time.Now().Add(30 * time.Minute).UnixMilli(),
	}
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	got, err := resolveCredentials(cfg, executorRequest{StorageJSON: raw})
	if err != nil {
		t.Fatalf("resolveCredentials error: %v", err)
	}
	if !strings.Contains(got.bearer, "still-good") {
		t.Fatalf("got %q, want access token preferred while fresh", got.bearer)
	}
	if got.source != "oauth" {
		t.Fatalf("source = %q, want oauth", got.source)
	}
}
