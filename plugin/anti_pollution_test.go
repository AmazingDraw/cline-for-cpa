package plugin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestKeyCredentialAntiPollution(t *testing.T) {
	dir := t.TempDir()
	cfg := pluginConfig{AuthDir: dir, CredentialPreference: "oauth_first"}

	// Stored data corrupted with OAuth tokens inside a key-only file
	st := &clineOAuthStorage{
		Type:         ProviderKey,
		APIKey:       "sk-testkey",
		AccessToken:  "polluted-access",
		RefreshToken: "polluted-refresh",
		ExpiresAt:    123456789,
	}

	// Persist to a key file
	path := filepath.Join(dir, "cline-key-monochord.json")
	if err := safeInPlaceWrite(path, []byte(`{"type":"cline","api_key":"sk-testkey"}`)); err != nil {
		t.Fatal(err)
	}

	// Persisting refreshed storage for key file must strip OAuth tokens
	if err := persistRefreshedStorage(cfg, st); err != nil {
		t.Fatal(err)
	}

	// Verify file content has NO tokens and priority is 0
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}

	if _, hasTok := data["refresh_token"]; hasTok {
		t.Error("refresh_token was NOT stripped from key file!")
	}
	if _, hasAcc := data["access_token"]; hasAcc {
		t.Error("access_token was NOT stripped from key file!")
	}
	if pri, ok := data["priority"].(float64); !ok || int(pri) != 0 {
		t.Errorf("priority for key file under oauth_first = %v, want 0", data["priority"])
	}
}
