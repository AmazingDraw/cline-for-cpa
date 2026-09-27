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

func TestSyncConfigAPIKeyCredentialDoesNotSeed(t *testing.T) {
	dir := t.TempDir()
	cfg := pluginConfig{
		AuthDir: dir,
		APIKey:  "sk_test_12345678abcd",
	}
	syncConfigAPIKeyCredential(cfg)
	targetFile := filepath.Join(dir, "cline-key-monochord.json")
	if _, err := os.Stat(targetFile); !os.IsNotExist(err) {
		t.Fatalf("oauth-only must not create %s", targetFile)
	}

	userFile := filepath.Join(dir, "cline-key-manual.json")
	if err := os.WriteFile(userFile, []byte(`{"type":"cline","api_key":"manual"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	syncConfigAPIKeyCredential(cfg)
	if _, err := os.Stat(userFile); err != nil {
		t.Fatalf("leftover key file must be left on disk: %v", err)
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
		t.Fatalf("expected oauth priority 1, got %v", envOAuth.Result.Auth.Attributes["priority"])
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
	if envKey.Result.Handled {
		t.Fatal("key-only files must not be handled in oauth-only mode")
	}

	pollutedOAuthJSON := []byte(`{"type":"cline","access_token":"token","refresh_token":"ref","api_key":"sk_polluted"}`)
	rawPollutedReq, _ := json.Marshal(map[string]any{"raw_json": pollutedOAuthJSON, "file_name": "cline-polluted@example.com.json"})
	resPolluted, _ := handleAuthParse(rawPollutedReq)
	var envPolluted parseResult
	json.Unmarshal(resPolluted, &envPolluted)
	if envPolluted.Result.Auth.Attributes["priority"] != "1" {
		t.Fatalf("expected polluted oauth priority 1, got %v", envPolluted.Result.Auth.Attributes["priority"])
	}
}
