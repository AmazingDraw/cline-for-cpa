package plugin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestIsPlausibleEmail(t *testing.T) {
	valid := []string{
		"user@example.com",
		"user@example.com",
		"a.b+c@domain.co.uk",
	}
	invalid := []string{
		"",
		"API Key ····ad9e",
		"nas-key",
		"user@",
		"@domain.com",
		"no-at-sign.com",
		"user@nodot",
	}
	for _, s := range valid {
		if !isPlausibleEmail(s) {
			t.Errorf("expected %q to be plausible email", s)
		}
	}
	for _, s := range invalid {
		if isPlausibleEmail(s) {
			t.Errorf("expected %q to be NOT plausible email", s)
		}
	}
}

func TestSyncConfigAPIKeyCredential(t *testing.T) {
	dir := t.TempDir()
	cfg := pluginConfig{
		AuthDir: dir,
		APIKeys: []string{"sk_test_12345678abcd"},
	}

	// 1. With APIKey set, credential file should be auto-created as Greek sequence name
	syncConfigAPIKeyCredential(cfg)
	targetFile := filepath.Join(dir, "cline-key-monochord.json")
	raw, err := os.ReadFile(targetFile)
	if err != nil {
		t.Fatalf("expected %s to exist: %v", targetFile, err)
	}

	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("unmarshal generated auth: %v", err)
	}
	if data["type"] != "cline" || data["api_key"] != "sk_test_12345678abcd" {
		t.Fatalf("unexpected content: %v", data)
	}
	meta, ok := data["metadata"].(map[string]any)
	if !ok || meta["managed_by"] != configManagedKeyMarker {
		t.Fatalf("expected managed_by marker, got: %v", data["metadata"])
	}

	// 2. When the key is cleared from the array, the config-managed file should be removed
	cfg.APIKeys = nil
	cfg.APIKey = ""
	syncConfigAPIKeyCredential(cfg)
	if _, err := os.Stat(targetFile); !os.IsNotExist(err) {
		t.Fatalf("expected %s to be removed when APIKey cleared", targetFile)
	}

	// 3. User manually created file without managed_by should NOT be removed
	userFile := filepath.Join(dir, "cline-key-manual.json")
	if err := os.WriteFile(userFile, []byte(`{"type":"cline","api_key":"manual"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	syncConfigAPIKeyCredential(cfg)
	if _, err := os.Stat(userFile); err != nil {
		t.Fatalf("expected user manual file to be preserved: %v", err)
	}
}

func TestEnrichKeyOnlyIdentityHealsUnplausibleEmail(t *testing.T) {
	dir := t.TempDir()
	cfg := pluginConfig{AuthDir: dir}

	// Stub accountLookup
	origLookup := accountLookup
	defer func() { accountLookup = origLookup }()
	accountLookup = func(cfg pluginConfig, bearer string) (*accountIdentity, error) {
		return &accountIdentity{
			Email:       "healed@example.com",
			DisplayName: "Healed User",
			UserID:      "usr-healed-123",
		}, nil
	}

	// Key with fake/tainted email like "API Key ····ad9e"
	st := clineOAuthStorage{
		Type:   ProviderKey,
		APIKey: "sk_fake_test_ad9e",
		Email:  "API Key ····ad9e",
	}

	enriched := enrichKeyOnlyIdentity(cfg, st)
	if enriched.Email != "healed@example.com" {
		t.Fatalf("expected healed email, got %q", enriched.Email)
	}
	if enriched.AccountID != "usr-healed-123" {
		t.Fatalf("expected healed user ID, got %q", enriched.AccountID)
	}
	if enriched.Metadata["name"] != "Healed User" {
		t.Fatalf("expected healed name, got %v", enriched.Metadata["name"])
	}

	// Valid email should NOT be looked up again
	called := false
	accountLookup = func(cfg pluginConfig, bearer string) (*accountIdentity, error) {
		called = true
		return nil, nil
	}
	validSt := clineOAuthStorage{
		Type:   ProviderKey,
		APIKey: "sk_valid_key",
		Email:  "already_valid@example.com",
	}
	enrichKeyOnlyIdentity(cfg, validSt)
	if called {
		t.Fatal("expected accountLookup not to be called for already valid email")
	}
}

func TestHandleAuthParseCredentialPreference(t *testing.T) {
	oauthJSON := `{"type":"cline","access_token":"at","refresh_token":"rt","email":"test@example.com"}`
	keyJSON := `{"type":"cline","api_key":"sk_test_1234","label":"API Key ····1234"}`

	type parseResult struct {
		OK     bool `json:"ok"`
		Result struct {
			Handled bool `json:"Handled"`
			Auth    struct {
				Attributes map[string]string `json:"Attributes"`
				Metadata   map[string]any    `json:"Metadata"`
			} `json:"Auth"`
		} `json:"result"`
	}

	// 1. Default: oauth_first
	activeConfig = defaultConfig()
	rawOAuthReq, _ := json.Marshal(map[string]any{"raw_json": oauthJSON, "file_name": "cline-test.json"})
	resOAuth, err := handleAuthParse(rawOAuthReq)
	if err != nil {
		t.Fatal(err)
	}
	var envOAuth parseResult
	if err := json.Unmarshal(resOAuth, &envOAuth); err != nil {
		t.Fatal(err)
	}
	if envOAuth.Result.Auth.Attributes["priority"] != "1" {
		t.Fatalf("expected oauth priority 1 under oauth_first, got %v", envOAuth.Result.Auth.Attributes["priority"])
	}

	rawKeyReq, _ := json.Marshal(map[string]any{"raw_json": keyJSON, "file_name": "cline-key-1234.json"})
	resKey, err := handleAuthParse(rawKeyReq)
	if err != nil {
		t.Fatal(err)
	}
	var envKey parseResult
	if err := json.Unmarshal(resKey, &envKey); err != nil {
		t.Fatal(err)
	}
	if envKey.Result.Auth.Attributes["priority"] != "0" {
		t.Fatalf("expected key priority 0 under oauth_first, got %v", envKey.Result.Auth.Attributes["priority"])
	}

	// 2. key_first
	activeConfig = defaultConfig()
	activeConfig.CredentialPreference = "key_first"
	resKey2, _ := handleAuthParse(rawKeyReq)
	json.Unmarshal(resKey2, &envKey)
	if envKey.Result.Auth.Attributes["priority"] != "1" {
		t.Fatalf("expected key priority 1 under key_first, got %v", envKey.Result.Auth.Attributes["priority"])
	}

	// 3. round_robin
	activeConfig = defaultConfig()
	activeConfig.CredentialPreference = "round_robin"
	resOAuth3, _ := handleAuthParse(rawOAuthReq)
	json.Unmarshal(resOAuth3, &envOAuth)
	if envOAuth.Result.Auth.Attributes["priority"] != "0" {
		t.Fatalf("expected oauth priority 0 under round_robin, got %v", envOAuth.Result.Auth.Attributes["priority"])
	}

	// 4. Polluted OAuth file containing both tokens and api_key must still resolve as priority 1 under oauth_first
	activeConfig = defaultConfig()
	activeConfig.CredentialPreference = "oauth_first"
	pollutedOAuthJSON := []byte(`{"type":"cline","access_token":"token","refresh_token":"ref","api_key":"sk_polluted"}`)
	rawPollutedReq, _ := json.Marshal(map[string]any{"raw_json": pollutedOAuthJSON, "file_name": "cline-polluted@example.com.json"})
	resPolluted, _ := handleAuthParse(rawPollutedReq)
	var envPolluted parseResult
	json.Unmarshal(resPolluted, &envPolluted)
	if envPolluted.Result.Auth.Attributes["priority"] != "1" {
		t.Fatalf("expected polluted oauth priority 1 under oauth_first, got %v", envPolluted.Result.Auth.Attributes["priority"])
	}
}
