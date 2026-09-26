package plugin

import (
	"encoding/json"
	"strings"
	"testing"
)

// hostModelInfo mirrors CLIProxyAPI pluginapi.ModelInfo (untagged) for ABI decode.
type hostModelInfo struct {
	ID      string
	Object  string
	OwnedBy string
	Type    string
}

type hostModelResponse struct {
	Provider string
	Models   []hostModelInfo
}

func TestStaticModelsOwnedBySurvivesABIRoundTrip(t *testing.T) {
	raw, err := HandleMethod("model.static", nil)
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if !env.OK {
		t.Fatalf("model.static not ok: %s", raw)
	}
	var resp hostModelResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Provider != ProviderKey {
		t.Fatalf("Provider=%q want %q", resp.Provider, ProviderKey)
	}
	if len(resp.Models) == 0 {
		t.Fatal("no models")
	}
	for _, m := range resp.Models {
		if m.OwnedBy != ProviderKey {
			t.Fatalf("model %q OwnedBy=%q want %q (PascalCase ABI required)", m.ID, m.OwnedBy, ProviderKey)
		}
		if m.ID == "" || m.Object != "model" || m.Type != ProviderKey {
			t.Fatalf("bad model %#v", m)
		}
	}

	forAuthRaw, err := HandleMethod("model.for_auth", nil)
	if err != nil {
		t.Fatal(err)
	}
	var forAuthEnv struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(forAuthRaw, &forAuthEnv); err != nil {
		t.Fatal(err)
	}
	if !forAuthEnv.OK {
		t.Fatalf("model.for_auth not ok: %s", forAuthRaw)
	}
	// nil/empty request gets all models by default for backward compatibility
	if string(forAuthEnv.Result) != string(env.Result) {
		t.Fatalf("model.for_auth payload differs from model.static on nil request")
	}
}

func TestModelForAuthFiltersPassModelsForFreeTier(t *testing.T) {
	// 1. Free tier OAuth credential request
	freeReq := []byte(`{
		"storage_json": "{\"type\":\"cline\",\"email\":\"free@example.com\",\"refresh_token\":\"rt\"}",
		"metadata": {"tier": "Free Tier"}
	}`)
	raw, err := HandleMethod("model.for_auth", freeReq)
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		OK     bool             `json:"ok"`
		Result abiModelResponse `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if !env.OK {
		t.Fatalf("model.for_auth failed: %s", raw)
	}
	for _, m := range env.Result.Models {
		if strings.HasPrefix(m.ID, "cline-pass/") {
			t.Fatalf("free tier credential advertised cline-pass model: %s", m.ID)
		}
	}

	// 2. Paid / Cline Pass credential request
	passReq := []byte(`{
		"storage_json": "{\"type\":\"cline\",\"email\":\"pass@example.com\",\"refresh_token\":\"rt\"}",
		"metadata": {"tier": "Cline Pass"}
	}`)
	rawPass, err := HandleMethod("model.for_auth", passReq)
	if err != nil {
		t.Fatal(err)
	}
	var envPass struct {
		OK     bool             `json:"ok"`
		Result abiModelResponse `json:"result"`
	}
	if err := json.Unmarshal(rawPass, &envPass); err != nil {
		t.Fatal(err)
	}
	hasPass := false
	for _, m := range envPass.Result.Models {
		if strings.HasPrefix(m.ID, "cline-pass/") {
			hasPass = true
			break
		}
	}
	if !hasPass {
		t.Fatal("paid credential has no cline-pass models")
	}

	// 3. API Key credential without Pass subscription must be classified as Free Tier
	freeKeyReq := []byte(`{
		"storage_json": "{\"type\":\"cline\",\"api_key\":\"sk_free_key_without_pass\"}"
	}`)
	rawFreeKey, err := HandleMethod("model.for_auth", freeKeyReq)
	if err != nil {
		t.Fatal(err)
	}
	var envFreeKey struct {
		OK     bool             `json:"ok"`
		Result abiModelResponse `json:"result"`
	}
	if err := json.Unmarshal(rawFreeKey, &envFreeKey); err != nil {
		t.Fatal(err)
	}
	for _, m := range envFreeKey.Result.Models {
		if strings.HasPrefix(m.ID, "cline-pass/") {
			t.Fatalf("free API key credential advertised cline-pass model: %s", m.ID)
		}
	}
}

func TestSnakeCaseOwnedByDoesNotPopulateUntaggedStruct(t *testing.T) {
	snake := []byte(`{"Provider":"cline","Models":[{"ID":"cline-pass/x","Object":"model","owned_by":"cline","Type":"cline"}]}`)
	var resp hostModelResponse
	if err := json.Unmarshal(snake, &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Models) != 1 {
		t.Fatalf("models=%d", len(resp.Models))
	}
	if resp.Models[0].OwnedBy != "" {
		t.Fatalf("expected empty OwnedBy from snake_case, got %q", resp.Models[0].OwnedBy)
	}
}
