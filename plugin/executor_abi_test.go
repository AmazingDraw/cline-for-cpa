package plugin

import (
	"encoding/json"
	"testing"
	"time"
)

func TestExecutorRequestReadsHostPascalCaseStorageJSON(t *testing.T) {
	storage, err := json.Marshal(clineOAuthStorage{
		Type:         ProviderKey,
		AccessToken:  "access-from-host",
		RefreshToken: "refresh-from-host",
		ExpiresAt:    time.Now().Add(time.Hour).UnixMilli(),
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{
		"Model":       "cline-pass/glm-5.3-flash",
		"Payload":     []byte(`{"model":"cline-pass/glm-5.3-flash"}`),
		"StorageJSON": storage,
	})
	if err != nil {
		t.Fatal(err)
	}

	var req executorRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatal(err)
	}
	req.normalizeCasing()
	if string(req.StorageJSON) != string(storage) {
		t.Fatalf("StorageJSON not populated from PascalCase wire")
	}

	got, err := resolveCredentials(defaultConfig(), req)
	if err != nil {
		t.Fatalf("resolveCredentials: %v", err)
	}
	if !got.oauth || got.bearer == "" {
		t.Fatalf("cred = %+v, want oauth bearer", got)
	}
}
