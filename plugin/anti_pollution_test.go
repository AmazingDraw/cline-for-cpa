package plugin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Leftover cline-key-*.json must not absorb OAuth tokens when something
// mistakenly persists into that path.
func TestKeyFileRejectsOAuthTokenWrite(t *testing.T) {
	dir := t.TempDir()
	cfg := pluginConfig{AuthDir: dir}
	path := filepath.Join(dir, "cline-key-monochord.json")
	if err := safeInPlaceWrite(path, []byte(`{"type":"cline","api_key":"sk-testkey"}`)); err != nil {
		t.Fatal(err)
	}
	st := &clineOAuthStorage{
		Type: ProviderKey, APIKey: "sk-testkey",
		AccessToken: "polluted-access", RefreshToken: "polluted-refresh", ExpiresAt: 123,
	}
	if err := persistRefreshedStorage(cfg, st); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"access_token", "refresh_token", "expires_at"} {
		if _, ok := data[k]; ok {
			t.Fatalf("%s must not land in key file: %s", k, raw)
		}
	}
}
