package plugin

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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

// The config api_key is a seed for credential files, never a runtime fallback.
// When refresh fails and the credential carries no key of its own, the turn
// must fail — silently billing the usage pool is what the plugin must never do.
func TestResolveCredentialsNeverFallsBackToConfigAPIKey(t *testing.T) {
	cfg := pluginConfig{APIKeys: []string{"sk_config_key"}, BaseURL: "http://127.0.0.1:1", AuthDir: t.TempDir()}
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
	if err == nil {
		t.Fatalf("expected an error, got credential with bearer %q — the config key must never be used as a runtime fallback", got.bearer)
	}
	if got.bearer != "" {
		t.Fatalf("bearer = %q, want empty: the config key (%q) must not reach the upstream through the executor", got.bearer, "sk_config_key")
	}
}

// No credential at all, config key present: the executor must report
// ErrNoCredentials rather than quietly adopting the config key.
func TestResolveCredentialsNoCredentialWithoutConfigFallback(t *testing.T) {
	cfg := pluginConfig{APIKeys: []string{"sk_config_key"}, BaseURL: "http://127.0.0.1:1", AuthDir: t.TempDir()}
	got, err := resolveCredentials(cfg, executorRequest{})
	if !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("err = %v, want ErrNoCredentials (bearer=%q)", err, got.bearer)
	}
	if got.bearer != "" {
		t.Fatalf("bearer = %q, want empty", got.bearer)
	}
}

// A credential the panel disabled must be refused even when the host still
// hands it over, and even when the flag only lives in the on-disk file (the
// check runs after the disk repair for exactly that reason).
func TestResolveCredentialsRefusesDisabledCredential(t *testing.T) {
	dir := t.TempDir()
	st := clineOAuthStorage{Type: ProviderKey, APIKey: "sk_disabled", Disabled: true}
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cline-key-monochord.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := pluginConfig{AuthDir: dir, BaseURL: "http://127.0.0.1:1"}

	// Flag carried by the handed-over record.
	got, err := resolveCredentials(cfg, executorRequest{StorageJSON: raw})
	if !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("inline flag: err = %v, want ErrNoCredentials (bearer=%q)", err, got.bearer)
	}

	// Flag only on disk: hand over a trimmed record without it.
	trimmed, err := json.Marshal(clineOAuthStorage{Type: ProviderKey, APIKey: "sk_disabled"})
	if err != nil {
		t.Fatal(err)
	}
	got, err = resolveCredentials(cfg, executorRequest{StorageJSON: trimmed})
	if !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("disk-only flag: err = %v, want ErrNoCredentials (bearer=%q)", err, got.bearer)
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
