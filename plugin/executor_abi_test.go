package plugin

import (
	"encoding/json"
	"testing"
)

// The host marshals pluginapi.ExecutorRequest with no JSON tags, so the wire
// keys are PascalCase (StorageJSON, AuthMetadata, Model, …). encoding/json's
// case-insensitive fallback matches "Model" to `json:"model"` but NOT
// "StorageJSON" to `json:"storage_json"` (the underscore survives folding).
// 0.3.24 removed the config-key runtime fallback that had been papering this
// over; without reading StorageJSON every turn looks like a missing credential.
func TestExecutorRequestReadsHostPascalCaseStorageJSON(t *testing.T) {
	storage, err := json.Marshal(clineOAuthStorage{Type: ProviderKey, APIKey: "sk_from_host"})
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

	if req.Model != "cline-pass/glm-5.3-flash" {
		t.Fatalf("Model = %q", req.Model)
	}
	if string(req.StorageJSON) != string(storage) {
		t.Fatalf("StorageJSON not populated from PascalCase wire: %q", req.StorageJSON)
	}

	got, err := resolveCredentials(defaultConfig(), req)
	if err != nil {
		t.Fatalf("resolveCredentials: %v", err)
	}
	if got.bearer != "sk_from_host" || got.source != "api_key" {
		t.Fatalf("cred = %+v, want bearer sk_from_host source api_key", got)
	}
}

func TestExecutorRequestStillReadsSnakeCaseStorageJSON(t *testing.T) {
	storage, err := json.Marshal(clineOAuthStorage{Type: ProviderKey, APIKey: "sk_snake"})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{"storage_json": storage})
	if err != nil {
		t.Fatal(err)
	}

	var req executorRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatal(err)
	}
	req.normalizeCasing()

	got, err := resolveCredentials(defaultConfig(), req)
	if err != nil {
		t.Fatalf("resolveCredentials: %v", err)
	}
	if got.bearer != "sk_snake" {
		t.Fatalf("bearer = %q", got.bearer)
	}
}
