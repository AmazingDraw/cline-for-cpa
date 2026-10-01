package plugin

import (
	"net/http"
	"slices"
	"strings"
	"testing"
)

// The free tier is a different upstream pool: rewriting cline-free/x into
// cline-pass/x would spend subscription usage on a free request (and vice versa,
// hide the free window behind the plan's window).
func TestNormalizeModelKeepsFreeNamespace(t *testing.T) {
	cases := map[string][2]string{
		"cline-free/deepseek-v4.1-flash":        {"cline-free/deepseek-v4.1-flash", "cline-free/deepseek-v4.1-flash"},
		"cline-free/mimo-v2.6-flash":            {"cline-free/mimo-v2.6-flash", "cline-free/mimo-v2.6-flash"},
		"cline-free/muse-spark-1.3-contributor": {"cline-free/muse-spark-1.3-contributor", "cline-free/muse-spark-1.3-contributor"},
		// bare ids keep the historical default
		"deepseek-v4.1-flash":           {"cline-pass/deepseek-v4.1-flash", "cline-pass/deepseek-v4.1-flash"},
		"cline-pass/kimi-k3":            {"cline-pass/kimi-k3", "cline-pass/kimi-k3"},
		"cline-pass/cline-pass/kimi-k3": {"cline-pass/kimi-k3", "cline-pass/kimi-k3"},
		// the first namespace seen wins; inner ones are peeled as noise
		"cline-free/cline-pass/kimi-k3": {"cline-free/kimi-k3", "cline-free/kimi-k3"},
		// vendor prefix in front of a model we publish is dropped
		"deepseek/deepseek-v4.1-flash": {"cline-pass/deepseek-v4.1-flash", "cline-pass/deepseek-v4.1-flash"},
		// unknown leaves still pass through, in their namespace
		"cline-free/whatever-v9": {"cline-free/whatever-v9", "cline-free/whatever-v9"},
	}
	for in, want := range cases {
		client, upstream, err := NormalizeModel(in)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if client != want[0] || upstream != want[1] {
			t.Fatalf("%s: client=%q upstream=%q want %q/%q", in, client, upstream, want[0], want[1])
		}
	}
	if _, _, err := NormalizeModel("  "); err == nil {
		t.Fatal("blank model id must be rejected")
	}
}

// Exposure is a **blacklist**: everything in a served namespace is advertised,
// so a model Cline adds shows up on its own; only the excluded ids are hidden.
// Unserved namespaces (recommended / cline-cloud) stay out.
func TestExposedModelsTiers(t *testing.T) {
	var pass, free, stealth []string
	for _, id := range StaticModelIDs() {
		switch {
		case strings.HasPrefix(id, namespacePass):
			pass = append(pass, id)
		case strings.HasPrefix(id, namespaceFree):
			free = append(free, id)
		case strings.HasPrefix(id, namespaceStealth):
			stealth = append(stealth, id)
		default:
			t.Fatalf("unexpected namespace in advertised list: %s", id)
		}
	}
	if len(pass) < 11 {
		t.Fatalf("clinePass models advertised = %d, want at least 11 (17 minus the 3 blacklisted)", len(pass))
	}
	// Floor tracks the live feed, not a historical peak. 2026-10-01 refresh
	// dropped stealth/pixel-canary (3 free + 1 stealth).
	if len(free)+len(stealth) < 4 {
		t.Fatalf("free/stealth models advertised = %d, want at least 4", len(free)+len(stealth))
	}
	if !slices.Contains(stealth, "stealth/space-bunny-alpha") {
		t.Fatalf("stealth/space-bunny-alpha missing from advertised models: %v", stealth)
	}
	// The blacklist is honoured (matched on the full id here)…
	for _, banned := range []string{
		"cline-pass/qwen3.8-max",
		"cline-pass/qwen3.7-max",
		"cline-pass/qwen3.7-plus",
	} {
		if _, ok := lookupModel(banned); ok {
			t.Fatalf("%s is blacklisted but still advertised", banned)
		}
	}
	// …the unserved tiers stay out…
	for _, banned := range []string{
		"cline-cloud/kimi-k3",
		"openai/gpt-6-astra",
		"spacexai/grok-4.7",
	} {
		if _, ok := lookupModel(banned); ok {
			t.Fatalf("%s belongs to an unserved tier and must not be advertised", banned)
		}
	}
	// …and everything else in a served namespace appears without a code change.
	for _, want := range []string{
		"cline-free/deepseek-v4.1-flash",
		"cline-free/mimo-v2.6-flash",
		"cline-free/muse-spark-1.3-contributor",
		"cline-pass/mimo-v2.6-pro",
		"cline-pass/glm-5.3",
	} {
		if _, ok := lookupModel(want); !ok {
			t.Fatalf("missing advertised model %s", want)
		}
	}
}

// Blacklist entry semantics: a namespaced entry hides exactly that id, a bare
// leaf hides the leaf everywhere.
func TestExcludedModelsSemantics(t *testing.T) {
	if !isExcludedModel("cline-pass/qwen3.7-max") {
		t.Fatal("full id must match")
	}
	if isExcludedModel("cline-free/qwen3.7-max") {
		t.Fatal("a namespaced entry must not leak into another namespace")
	}
	if isExcludedModel("cline-pass/kimi-k3") {
		t.Fatal("unrelated model must not be excluded")
	}

	saved := excludedModels
	defer func() { excludedModels = saved }()
	excludedModels = []string{"kimi-k3"}
	if !isExcludedModel("cline-pass/kimi-k3") || !isExcludedModel("cline-free/kimi-k3") {
		t.Fatal("a bare leaf must match in any namespace")
	}
	if isExcludedModel("cline-pass/mimo-v2.6-pro") {
		t.Fatal("a bare leaf must not match a different model")
	}
}

// Metadata is only useful if it reaches the host, so both the catalog lookup and
// the ABI payload are asserted.
func TestModelMetadataReachesABIPayload(t *testing.T) {
	mimo, ok := lookupModel("cline-free/mimo-v2.6-flash")
	if !ok {
		t.Fatal("free mimo model missing from the catalog")
	}
	if mimo.Meta.ContextLength <= 0 || mimo.Meta.MaxOutputTokens <= 0 || mimo.Meta.DisplayName == "" {
		t.Fatalf("mimo-v2.6-flash metadata incomplete: %+v", mimo.Meta)
	}

	// Generated rows must not regress to blank windows after a refresh.
	for _, m := range exposedModels() {
		if m.Meta.ContextLength <= 0 {
			t.Fatalf("%s: no context length in the generated catalog (re-run `go run ./tools/modelmeta`)", m.ID)
		}
		if m.Meta.MaxOutputTokens <= 0 {
			t.Fatalf("%s: no max output tokens in the generated catalog", m.ID)
		}
	}

	var seen bool
	for _, m := range staticModels().Models {
		if m.ID != "cline-free/mimo-v2.6-flash" {
			continue
		}
		seen = true
		if m.OwnedBy != ProviderKey || m.Type != ProviderKey {
			t.Fatalf("OwnedBy/Type must stay %q or the host drops the model: %+v", ProviderKey, m)
		}
		if m.ContextLength != mimo.Meta.ContextLength || m.MaxCompletionTokens != mimo.Meta.MaxOutputTokens {
			t.Fatalf("ABI window fields not populated: ctx=%d maxOut=%d lookup ctx=%d maxOut=%d",
				m.ContextLength, m.MaxCompletionTokens, mimo.Meta.ContextLength, mimo.Meta.MaxOutputTokens)
		}
		if m.InputTokenLimit != m.ContextLength || m.OutputTokenLimit != m.MaxCompletionTokens {
			t.Fatalf("legacy window fields disagree with ContextLength/MaxCompletionTokens: %+v", m)
		}
		if len(m.SupportedInputModalities) == 0 || m.DisplayName == "" {
			t.Fatalf("modalities/display name not populated: %+v", m)
		}
	}
	if !seen {
		t.Fatal("free mimo model missing from the ABI payload")
	}
}

// Cline reports tier limits in the message, not the status — a plain 4xx path
// would tell the user nothing about which pool ran dry or when it refills.
func TestTierLimitBodiesBecomeRetryable429(t *testing.T) {
	free := ClassifyUpstreamHTTP(http.StatusBadRequest,
		`{"error":"free limit reached on model cline-free/deepseek-v4.1-flash, try again in 2h 30m","success":false}`)
	if free.status != http.StatusTooManyRequests || free.code != "cline_free_limit" {
		t.Fatalf("free limit classified as %d/%q", free.status, free.code)
	}
	if !strings.Contains(free.message, "2h 30m") {
		t.Fatalf("reset hint lost: %s", free.message)
	}
	if free.retryable == nil || !*free.retryable {
		t.Fatal("tier limits must be retryable")
	}
	if strings.Contains(free.message, `"success"`) {
		t.Fatalf("JSON punctuation leaked into the message: %s", free.message)
	}

	pass := ClassifyUpstreamHTTP(http.StatusTooManyRequests,
		`{"error":"you have reached your clinepass limit, please try again later."}`)
	if pass.code != "cline_pass_limit" {
		t.Fatalf("clinepass limit classified as %q", pass.code)
	}

	// Unrelated failures keep the generic path (empty code → host-style code).
	other := ClassifyUpstreamHTTP(http.StatusBadRequest, `{"error":"bad request"}`)
	if other.code != "" {
		t.Fatalf("unrelated 400 must not be reclassified, got %q", other.code)
	}
}

// Panel card title: identity / override / OAuth fallback.
func TestCredentialLabel(t *testing.T) {
	if got := credentialLabel(nil); got != "Cline OAuth" {
		t.Fatalf("nil=%q", got)
	}
	st := &clineOAuthStorage{Email: "user@example.com", AccessToken: "x"}
	if got := credentialLabel(st); got != "user@example.com" {
		t.Fatalf("oauth label=%q", got)
	}
	st.Metadata = map[string]any{"label": "Custom Key"}
	if got := credentialLabel(st); got != "Custom Key" {
		t.Fatalf("override=%q", got)
	}
}

func TestCredentialFileName(t *testing.T) {
	if got := credentialFileName(nil); got != defaultAuthFileName {
		t.Fatalf("nil=%q", got)
	}
	oauth := &clineOAuthStorage{Email: "user@example.com", AccessToken: "x"}
	if got := credentialFileName(oauth); got != "cline-user@example.com.json" {
		t.Fatalf("oauth file name=%q", got)
	}
	// Leftover APIKey on an OAuth record must not divert to cline-key-*.
	oauth.APIKey = "sk-leftover"
	if got := credentialFileName(oauth); got != "cline-user@example.com.json" {
		t.Fatalf("leftover key diverted name=%q", got)
	}
	if got := credentialFileName(&clineOAuthStorage{}); got != defaultAuthFileName {
		t.Fatalf("identity-less=%q", got)
	}
}
