package plugin

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
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
	if len(free)+len(stealth) < 6 {
		t.Fatalf("free/stealth models advertised = %d, want at least 6", len(free)+len(stealth))
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
	if mimo.Meta.ContextLength != 1048576 {
		t.Fatalf("mimo-v2.6-flash context = %d, want 1048576", mimo.Meta.ContextLength)
	}
	if mimo.Meta.MaxOutputTokens <= 0 || mimo.Meta.DisplayName == "" {
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
		if m.ContextLength != 1048576 || m.MaxCompletionTokens != 131072 {
			t.Fatalf("ABI window fields not populated: ctx=%d maxOut=%d", m.ContextLength, m.MaxCompletionTokens)
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

// Panel card title rule: identity, never the provider (the panel already shows a
// "Cline" chip), and never two identical cards for two different keys.
func TestCredentialLabel(t *testing.T) {
	oauth := &clineOAuthStorage{Email: "user@example.com", AccessToken: "x"}
	if got := credentialLabel(oauth, false, true); got != "user@example.com" {
		t.Fatalf("oauth label = %q", got)
	}
	key := &clineOAuthStorage{APIKey: "sk-test-fixture-aaaaaaaaaaaaaaaaaaaaafa21"}
	if got := credentialLabel(key, true, false); got != "API Key ····fa21" {
		t.Fatalf("key label = %q", got)
	}
	override := &clineOAuthStorage{
		Email: "user@example.com", APIKey: "sk-test-fixture-aaaaaaaaaaaaaaaaaaaaafa21",
		Metadata: map[string]any{"label": "Custom Key"},
	}
	if got := credentialLabel(override, true, false); got != "Custom Key" {
		t.Fatalf("explicit label must win, got %q", got)
	}
	if got := credentialLabel(nil, true, false); got != "Cline API Key" {
		t.Fatalf("key fallback = %q", got)
	}
}

// A key-only credential must never take its file name from the display label:
// "API Key ····fa21" contains spaces and dots, and that name ends up in a path
// (and in the card subtitle). Regression guard for the 0.3.0 label change.
func TestCredentialFileName(t *testing.T) {
	oauth := &clineOAuthStorage{Email: "user@example.com", AccessToken: "x"}
	if got := credentialFileName(oauth); got != "cline-user@example.com.json" {
		t.Fatalf("oauth file name = %q", got)
	}
	key := &clineOAuthStorage{APIKey: "sk-test-fixture-aaaaaaaaaaaaaaaaaaaaafa21"}
	if got := credentialFileName(key); !isGreekKeySequenceName(got) && !strings.HasPrefix(got, "cline-key-") {
		t.Fatalf("key file name = %q, want key sequence name", got)
	}
	// A key-only credential that learned its email keeps the key slug: two keys
	// on one account must not collide on one file.
	keyKnown := &clineOAuthStorage{
		APIKey: "sk-test-fixture-aaaaaaaaaaaaaaaaaaaaafa21",
		Email:  "user@example.com",
	}
	if got := credentialFileName(keyKnown); got == "cline-user@example.com.json" {
		t.Fatalf("key-only file name flipped to the email: %q", got)
	}
	// No identity at all → the generic, plugin-scoped name; it must never encode
	// where the credential runs.
	if got := credentialFileName(nil); got != "cline.json" {
		t.Fatalf("generic file name = %q", got)
	}
	if got := credentialFileName(&clineOAuthStorage{}); got != "cline.json" {
		t.Fatalf("identity-less credential file name = %q", got)
	}
	// Two different keys must not collide.
	dir := t.TempDir()
	configMu.Lock()
	activeConfig.AuthDir = dir
	configMu.Unlock()
	a := credentialFileName(&clineOAuthStorage{APIKey: "sk-aaaa11"})
	_ = os.WriteFile(filepath.Join(dir, a), []byte(`{"api_key":"sk-aaaa11"}`), 0o600)
	b := credentialFileName(&clineOAuthStorage{APIKey: "sk-bbbb22"})
	if a == b {
		t.Fatalf("distinct keys collided on %q", a)
	}
}

// A bare API key can still learn its account: /users/me answers for sk_… too.
func TestKeyOnlyIdentityEnrichment(t *testing.T) {
	original := accountLookup
	defer func() { accountLookup = original }()
	accountMu.Lock()
	accountCache = map[string]cachedIdentity{}
	accountMu.Unlock()

	calls := 0
	accountLookup = func(cfg pluginConfig, bearer string) (*accountIdentity, error) {
		calls++
		if bearer == "" {
			t.Fatal("lookup must carry the key as bearer")
		}
		return &accountIdentity{Email: "user@example.com", DisplayName: "Test User", UserID: "usr-1"}, nil
	}
	cfg := pluginConfig{AuthDir: t.TempDir(), BaseURL: "https://example.invalid/api/v1"}

	st := enrichKeyOnlyIdentity(cfg, clineOAuthStorage{Type: ProviderKey, APIKey: "sk-abcdef1234"})
	if st.Email != "user@example.com" || st.AccountID != "usr-1" {
		t.Fatalf("identity not applied: %+v", st)
	}
	if got := credentialLabel(&st, true, false); got != "user@example.com" {
		t.Fatalf("card label after enrichment = %q", got)
	}
	if got := credentialFileName(&st); !strings.HasPrefix(got, "cline-key-") || got == "cline-user@example.com.json" {
		t.Fatalf("file name after enrichment = %q (must stay the key slug)", got)
	}
	// A second call is served from the cache.
	enrichKeyOnlyIdentity(cfg, clineOAuthStorage{Type: ProviderKey, APIKey: "sk-abcdef1234"})
	if calls != 1 {
		t.Fatalf("lookup calls = %d, want 1 (cached)", calls)
	}
	// An explicit label still wins over the resolved email.
	st.Metadata = map[string]any{"label": "Custom Key"}
	if got := credentialLabel(&st, true, false); got != "Custom Key" {
		t.Fatalf("explicit label must win, got %q", got)
	}

	// Failures are non-fatal and leave the credential untouched.
	accountMu.Lock()
	accountCache = map[string]cachedIdentity{}
	accountMu.Unlock()
	accountLookup = func(cfg pluginConfig, bearer string) (*accountIdentity, error) {
		return nil, errors.New("boom")
	}
	plain := enrichKeyOnlyIdentity(cfg, clineOAuthStorage{Type: ProviderKey, APIKey: "sk-zzzz9999"})
	if plain.Email != "" {
		t.Fatalf("failed lookup must not invent an identity: %+v", plain)
	}
	if got := credentialLabel(&plain, true, false); got != "API Key ····9999" {
		t.Fatalf("label after failed lookup = %q", got)
	}
}
