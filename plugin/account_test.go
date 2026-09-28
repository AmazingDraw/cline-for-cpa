package plugin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestIsPlausibleEmail(t *testing.T) {
	ok := []string{"a@b.co", "user.name+tag@example.com"}
	for _, s := range ok {
		if !isPlausibleEmail(s) {
			t.Fatalf("expected plausible: %q", s)
		}
	}
	bad := []string{"", "nope", "@x.com", "a@", "a@b", "a@b."}
	for _, s := range bad {
		if isPlausibleEmail(s) {
			t.Fatalf("expected implausible: %q", s)
		}
	}
}

func TestSyncAuthFilePrioritiesDoesNotSeedKeys(t *testing.T) {
	dir := t.TempDir()
	cfg := defaultConfig()
	cfg.AuthDir = dir
	cfg.APIKey = "sk_should_not_seed"
	syncAuthFilePriorities(cfg, dir)
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("oauth-only must not seed key files, got %d", len(entries))
	}

	// Hygiene: strip leaked api_key from an OAuth auth file.
	oauthPath := filepath.Join(dir, "cline-user@example.com.json")
	raw, _ := json.Marshal(map[string]any{
		"type": ProviderKey, "access_token": "at", "refresh_token": "rt",
		"api_key": "sk_leaked", "email": "user@example.com",
	})
	if err := os.WriteFile(oauthPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	syncAuthFilePriorities(cfg, dir)
	got, _ := os.ReadFile(oauthPath)
	var data map[string]any
	if err := json.Unmarshal(got, &data); err != nil {
		t.Fatal(err)
	}
	if _, ok := data["api_key"]; ok {
		t.Fatalf("api_key should be stripped from OAuth file: %s", got)
	}
}

func TestHandleAuthParseRejectsKeyOnly(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{
		"raw_json":  `{"type":"cline","api_key":"sk_only"}`,
		"file_name": "cline-key-monochord.json",
	})
	out, err := HandleMethod("auth.parse", raw)
	if err != nil {
		t.Fatal(err)
	}
	env := decodeEnvelope(t, out)
	if !env.OK {
		t.Fatalf("%+v", env.Error)
	}
	var result map[string]any
	if err := json.Unmarshal(env.Result, &result); err != nil {
		t.Fatal(err)
	}
	if result["Handled"] != false {
		t.Fatalf("key-only must be Handled=false, got %v", result)
	}
}
