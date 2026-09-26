package plugin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPersistRefreshedStorageWritesRotatedToken(t *testing.T) {
	dir := t.TempDir()
	cfg := pluginConfig{AuthDir: dir}
	email := "persist@example.com"
	path := filepath.Join(dir, clineAuthFileName(email))
	// Host-stored file with extra metadata that must survive the merge.
	initial := map[string]any{
		"type": "cline", "access_token": "old", "refresh_token": "rt-old",
		"expires_at": time.Now().Add(-time.Hour).UnixMilli(), "email": email,
		"host_extra": "keep-me",
	}
	raw, _ := json.Marshal(initial)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	fresh := &clineOAuthStorage{
		Type: ProviderKey, AccessToken: "new-access", RefreshToken: "rt-new",
		ExpiresAt: time.Now().Add(55 * time.Minute).UnixMilli(), Email: email,
		AccountID: "usr-1",
	}
	if err := persistRefreshedStorage(cfg, fresh); err != nil {
		t.Fatalf("persist: %v", err)
	}
	var got map[string]any
	back, _ := os.ReadFile(path)
	if err := json.Unmarshal(back, &got); err != nil {
		t.Fatal(err)
	}
	if got["refresh_token"] != "rt-new" || got["access_token"] != "new-access" {
		t.Fatalf("rotated credential not persisted: %+v", got)
	}
	if got["host_extra"] != "keep-me" {
		t.Fatalf("host metadata lost on merge: %+v", got)
	}
	if fi, err := os.Stat(path); err == nil && fi.Mode().Perm() != 0o600 {
		t.Fatalf("perm = %v, want 0600", fi.Mode().Perm())
	}
}

func TestPersistDoesNotBlankExistingFields(t *testing.T) {
	dir := t.TempDir()
	cfg := pluginConfig{AuthDir: dir}
	email := "merge@example.com"
	path := filepath.Join(dir, clineAuthFileName(email))
	raw, _ := json.Marshal(map[string]any{"email": email, "api_key": "sk_keep"})
	_ = os.WriteFile(path, raw, 0o600)

	fresh := &clineOAuthStorage{Type: ProviderKey, Email: email, AccessToken: "a", RefreshToken: "r"}
	if err := persistRefreshedStorage(cfg, fresh); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	back, _ := os.ReadFile(path)
	_ = json.Unmarshal(back, &got)
	if got["api_key"] != "sk_keep" {
		t.Fatalf("existing api_key blanked: %+v", got)
	}
}

func TestSafeInPlaceWritePreservesFileWithoutRename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	initial := []byte(`{"initial": true}`)
	if err := os.WriteFile(path, initial, 0o600); err != nil {
		t.Fatal(err)
	}

	updated := []byte(`{"updated": true}`)
	if err := safeInPlaceWrite(path, updated); err != nil {
		t.Fatalf("safeInPlaceWrite: %v", err)
	}

	back, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(back) != string(updated) {
		t.Fatalf("got %q, want %q", string(back), string(updated))
	}
}

func TestAuthRefreshPreservesHostAttributes(t *testing.T) {
	dir := t.TempDir()
	cfg := pluginConfig{AuthDir: dir}
	configMu.Lock()
	activeConfig = cfg
	configMu.Unlock()

	email := "attrs@example.com"
	storage := clineOAuthStorage{
		Type:         ProviderKey,
		Email:        email,
		AccessToken:  "access-tok",
		RefreshToken: "refresh-tok",
		ExpiresAt:    time.Now().Add(2 * time.Hour).UnixMilli(),
	}
	storageRaw, _ := json.Marshal(storage)

	req := map[string]any{
		"AuthID":      clineAuthFileName(email),
		"StorageJSON": storageRaw,
		"Attributes": map[string]string{
			"path":           filepath.Join(dir, clineAuthFileName(email)),
			"source":         filepath.Join(dir, clineAuthFileName(email)),
			"source_backend": "file",
			"custom_attr":    "keep-me",
		},
	}
	reqRaw, _ := json.Marshal(req)

	respRaw, err := handleAuthRefresh(reqRaw)
	if err != nil {
		t.Fatalf("handleAuthRefresh error: %v", err)
	}

	var env struct {
		Result struct {
			Auth struct {
				Attributes map[string]string `json:"Attributes"`
				ID         string            `json:"ID"`
			} `json:"Auth"`
		} `json:"result"`
	}
	if err := json.Unmarshal(respRaw, &env); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	attrs := env.Result.Auth.Attributes
	if attrs["path"] != filepath.Join(dir, clineAuthFileName(email)) {
		t.Fatalf("path attribute lost: got %q", attrs["path"])
	}
	if attrs["source"] != filepath.Join(dir, clineAuthFileName(email)) {
		t.Fatalf("source attribute lost: got %q", attrs["source"])
	}
	if attrs["source_backend"] != "file" {
		t.Fatalf("source_backend lost: got %q", attrs["source_backend"])
	}
	if attrs["custom_attr"] != "keep-me" {
		t.Fatalf("custom_attr lost: got %q", attrs["custom_attr"])
	}
	if attrs["priority"] == "" {
		t.Fatal("priority attribute missing")
	}
}
