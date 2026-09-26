package plugin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLiveModelsSnapshotParsing(t *testing.T) {
	feedJSON := `{
		"recommended": [{"id": "anthropic/claude-opus-5", "name": "claude-opus-5"}],
		"free": [
			{"id": "cline-free/gemini-3.8-flash", "name": "Gemini 3.8 Flash"},
			{"id": "stealth/space-bunny-alpha", "name": "space-bunny-alpha"}
		],
		"clinePass": [
			{"id": "cline-pass/mimo-v2.6-flash", "name": "cline-pass/mimo-v2.6-flash"}
		],
		"clineCloud": []
	}`

	catJSON := `{
		"data": [
			{
				"id": "google/gemini-3.8-flash",
				"context_length": 1048576,
				"top_provider": {"max_completion_tokens": 65536},
				"architecture": {"input_modalities": ["text", "image"], "output_modalities": ["text"]},
				"supported_parameters": ["tools", "temperature"]
			}
		]
	}`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ai/cline/recommended-models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(feedJSON))
		case "/ai/cline/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(catJSON))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	snap, err := fetchLiveModelsSnapshot(server.URL, "test-token")
	if err != nil {
		t.Fatalf("fetchLiveModelsSnapshot failed: %v", err)
	}

	gemini, ok := snap.Models["cline-free/gemini-3.8-flash"]
	if !ok {
		t.Fatal("missing cline-free/gemini-3.8-flash in snapshot")
	}
	if gemini.Tier != "free" {
		t.Fatalf("tier = %q, want free", gemini.Tier)
	}
	if gemini.ContextLength != 1048576 {
		t.Fatalf("context_length = %d, want 1048576", gemini.ContextLength)
	}
	if gemini.MaxOutputTokens != 65536 {
		t.Fatalf("max_output_tokens = %d, want 65536", gemini.MaxOutputTokens)
	}
	if gemini.Underlying != "google/gemini-3.8-flash" {
		t.Fatalf("underlying = %q, want google/gemini-3.8-flash", gemini.Underlying)
	}
}

func TestDynamicModelsDiscoveryFallback(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "models-cache-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	customModelsPath = filepath.Join(tmpDir, "models-cache.json")
	defer func() { customModelsPath = "" }()

	// Initially disk is empty, exposedModels should contain static models
	exposed := exposedModels()
	if len(exposed) == 0 {
		t.Fatal("exposedModels should not be empty")
	}

	// Inject a dynamic new model into cache and save to disk
	snap := modelsSnapshot{
		FetchedAt: time.Now(),
		Models: map[string]modelMeta{
			"cline-free/alien-super-v1": {
				Tier:          "free",
				DisplayName:   "Alien Super V1",
				ContextLength: 2000000,
			},
		},
		Order: []string{"cline-free/alien-super-v1"},
	}
	raw, _ := json.Marshal(snap)
	_ = os.WriteFile(customModelsPath, raw, 0o600)

	// Reload from disk
	loadModelsSnapshotFromDisk()
	rebuildModelIndex()

	m, ok := lookupModel("cline-free/alien-super-v1")
	if !ok {
		t.Fatal("dynamically injected alien-super-v1 model should be found in lookupModel")
	}
	if m.Meta.ContextLength != 2000000 {
		t.Fatalf("expected context length 2000000, got %d", m.Meta.ContextLength)
	}
}
