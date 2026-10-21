package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// ---------------------------------------------------------------------------
// fake Cline API
// ---------------------------------------------------------------------------

type fakeAPI struct {
	mu       sync.Mutex
	feed     map[string][]feedEntry
	catalog  []catalogEntry
	status   int // non-zero forces this status on every request
	token    string
	lastHdrs http.Header
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	f := &fakeAPI{token: "test-token", feed: fixtureFeed(), catalog: fixtureCatalog()}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.lastHdrs = r.Header.Clone()
		if f.status != 0 {
			http.Error(w, `{"error":"forced"}`, f.status)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+f.token {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/v1" + feedPath:
			_ = json.NewEncoder(w).Encode(f.feed)
		case "/api/v1" + catalogPath:
			_ = json.NewEncoder(w).Encode(map[string]any{"data": f.catalog})
		default:
			http.NotFound(w, r)
		}
	}))
	old := baseURL
	baseURL = srv.URL + "/api/v1"
	t.Cleanup(func() { baseURL = old; srv.Close() })
	return f
}

func (f *fakeAPI) set(fn func(f *fakeAPI)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func cat(id string, ctx, out int64, in ...string) catalogEntry {
	return catalogEntry{
		ID:            id,
		ContextLength: ctx,
		Architecture:  map[string]any{"input_modalities": toAny(in), "output_modalities": []any{"text"}},
		Supported:     []string{"max_tokens", "tools"},
		TopProvider:   map[string]any{"max_completion_tokens": float64(out)},
	}
}

func toAny(ss []string) []any {
	out := make([]any, 0, len(ss))
	for _, s := range ss {
		out = append(out, s)
	}
	return out
}

func fixtureFeed() map[string][]feedEntry {
	return map[string][]feedEntry{
		"recommended": {{ID: "vendora/model-x", Name: "Model X", Description: "x\nline two"}},
		"free":        {{ID: "cline-free/flash-y:free", Name: "Flash Y"}},
		"clinePass":   {{ID: "cline-pass/qwen3.8-max", Name: "Qwen Max"}, {ID: "cline-pass/dated", Name: "Dated"}},
		"clineCloud":  {{ID: "cline-cloud/z", Name: "Z"}},
	}
}

func fixtureCatalog() []catalogEntry {
	return []catalogEntry{
		cat("vendora/model-x", 200000, 64000, "text", "image"),
		cat("vendorb/model-x", 100000, 8000, "text"),
		cat("vendorc/flash-y", 1000000, 65536, "text"),
		cat("qwen/qwen3.8-max-0902", 1000000, 131072, "text"),
		cat("qwen/qwen3.8-max", 32000, 8000, "text"), // override must win over this exact match
		cat("vendord/dated-2026-09", 400000, 32000, "text"),
		cat("vendore/z", 50000, 4000, "text"),
	}
}

func runTool(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(args, &out, &errb)
	return code, out.String(), errb.String()
}

// ---------------------------------------------------------------------------
// run / runCheck exit codes
// ---------------------------------------------------------------------------

func TestRunWriteThenCheckUpToDate(t *testing.T) {
	f := newFakeAPI(t)
	gen := filepath.Join(t.TempDir(), "plugin", "modelmeta_gen.go")

	code, stdout, stderr := runTool(t, "--token", f.token, "--out", gen, "-v")
	if code != 0 {
		t.Fatalf("write exit=%d stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, "wrote "+gen+" (5 rows)") || !strings.Contains(stdout, "override") {
		t.Fatalf("unexpected stdout:\n%s", stdout)
	}
	src, err := os.ReadFile(gen)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"cline-pass/qwen3.8-max"`, `"qwen/qwen3.8-max-0902"`, "package plugin", "generatedOrder"} {
		if !strings.Contains(string(src), want) {
			t.Fatalf("generated file missing %q", want)
		}
	}

	// Identity headers sent to the API are the official CLI identity.
	h := f.lastHdrs
	if h.Get("User-Agent") != "Cline/"+clientVersion || h.Get("X-Client-Type") != "cline-cli" ||
		h.Get("X-Client-Version") != clientVersion || h.Get("X-Core-Version") != coreVersion ||
		h.Get("X-Platform") != "cli" || h.Get("X-IS-MULTIROOT") != "false" {
		t.Fatalf("identity headers: %v", h)
	}

	code, stdout, _ = runTool(t, "--token", f.token, "--out", gen, "--check")
	if code != 0 || !strings.Contains(stdout, "up to date (5 rows)") {
		t.Fatalf("check exit=%d stdout=%s", code, stdout)
	}
	after, _ := os.ReadFile(gen)
	if !bytes.Equal(src, after) {
		t.Fatal("--check must not rewrite the file")
	}
}

func TestRunCheckHardDriftExits1(t *testing.T) {
	f := newFakeAPI(t)
	gen := filepath.Join(t.TempDir(), "gen.go")
	if code, _, stderr := runTool(t, "--token", f.token, "--out", gen); code != 0 {
		t.Fatalf("seed: %s", stderr)
	}
	f.set(func(f *fakeAPI) { f.feed["clinePass"] = f.feed["clinePass"][:1] }) // model removed
	code, _, stderr := runTool(t, "--token", f.token, "--out", gen, "--check")
	if code != 1 || !strings.Contains(stderr, "HARD drift") || !strings.Contains(stderr, "model removed: cline-pass/dated") {
		t.Fatalf("want hard drift exit 1, got %d\n%s", code, stderr)
	}
}

func TestRunCheckSoftDriftExits0(t *testing.T) {
	f := newFakeAPI(t)
	gen := filepath.Join(t.TempDir(), "gen.go")
	if code, _, stderr := runTool(t, "--token", f.token, "--out", gen); code != 0 {
		t.Fatalf("seed: %s", stderr)
	}
	f.set(func(f *fakeAPI) { f.catalog[2].ContextLength = 999 })
	code, stdout, stderr := runTool(t, "--token", f.token, "--out", gen, "--check")
	if code != 0 || !strings.Contains(stderr, "WARN soft drift only") || !strings.Contains(stdout, "no hard drift") {
		t.Fatalf("want soft drift exit 0, got %d\n%s\n%s", code, stdout, stderr)
	}
}

func TestRunCheckFormattingOnlyExits0(t *testing.T) {
	f := newFakeAPI(t)
	gen := filepath.Join(t.TempDir(), "gen.go")
	if code, _, stderr := runTool(t, "--token", f.token, "--out", gen); code != 0 {
		t.Fatalf("seed: %s", stderr)
	}
	src, _ := os.ReadFile(gen)
	if err := os.WriteFile(gen, append(src, []byte("\n// trailing comment\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := runTool(t, "--token", f.token, "--out", gen, "--check")
	if code != 0 || !strings.Contains(stderr, "differs only in formatting") {
		t.Fatalf("want formatting-only exit 0, got %d\n%s", code, stderr)
	}
}

func TestRunCheckMissingOrBrokenFileExits1(t *testing.T) {
	f := newFakeAPI(t)
	dir := t.TempDir()
	if code, _, stderr := runTool(t, "--token", f.token, "--out", filepath.Join(dir, "absent.go"), "--check"); code != 1 ||
		!strings.Contains(stderr, "read ") {
		t.Fatalf("missing file: exit=%d %s", code, stderr)
	}
	broken := filepath.Join(dir, "broken.go")
	_ = os.WriteFile(broken, []byte("package plugin\nvar x = {{{\n"), 0o644)
	if code, _, stderr := runTool(t, "--token", f.token, "--out", broken, "--check"); code != 1 ||
		!strings.Contains(stderr, "treating as hard drift") {
		t.Fatalf("broken file: exit=%d %s", code, stderr)
	}
}

func TestRunHTTPErrorsExit1(t *testing.T) {
	f := newFakeAPI(t)
	gen := filepath.Join(t.TempDir(), "gen.go")
	if code, _, stderr := runTool(t, "--token", "wrong", "--out", gen); code != 1 || !strings.Contains(stderr, "fetch feed: HTTP 401") {
		t.Fatalf("bad token: exit=%d %s", code, stderr)
	}
	f.set(func(f *fakeAPI) { f.status = http.StatusBadGateway })
	if code, _, stderr := runTool(t, "--token", f.token, "--out", gen); code != 1 || !strings.Contains(stderr, "HTTP 502") {
		t.Fatalf("502: exit=%d %s", code, stderr)
	}
	if _, err := os.Stat(gen); !os.IsNotExist(err) {
		t.Fatal("a failed fetch must not write the file")
	}
}

func TestRunCatalogErrorExit1(t *testing.T) {
	f := newFakeAPI(t)
	old := baseURL
	// Feed served, catalog path 404s.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, feedPath) {
			_ = json.NewEncoder(w).Encode(fixtureFeed())
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	baseURL = srv.URL
	defer func() { baseURL = old }()
	if code, _, stderr := runTool(t, "--token", f.token, "--out", filepath.Join(t.TempDir(), "g.go")); code != 1 ||
		!strings.Contains(stderr, "fetch catalog: HTTP 404") {
		t.Fatalf("exit=%d %s", code, stderr)
	}
}

func TestRunBadJSONExit1(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()
	old := baseURL
	baseURL = srv.URL
	defer func() { baseURL = old }()
	if code, _, stderr := runTool(t, "--token", "x", "--out", filepath.Join(t.TempDir(), "g.go")); code != 1 ||
		!strings.Contains(stderr, "fetch feed") {
		t.Fatalf("exit=%d %s", code, stderr)
	}
}

func TestRunBadFlagExits2(t *testing.T) {
	if code, _, _ := runTool(t, "--no-such-flag"); code != 2 {
		t.Fatalf("exit=%d", code)
	}
}

func TestRunWriteErrorExit1(t *testing.T) {
	f := newFakeAPI(t)
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file")
	_ = os.WriteFile(blocker, nil, 0o644)
	if code, _, stderr := runTool(t, "--token", f.token, "--out", filepath.Join(blocker, "sub", "g.go")); code != 1 ||
		!strings.Contains(stderr, "mkdir") {
		t.Fatalf("exit=%d %s", code, stderr)
	}
	if code, _, stderr := runTool(t, "--token", f.token, "--out", dir); code != 1 || !strings.Contains(stderr, "write ") {
		t.Fatalf("exit=%d %s", code, stderr)
	}
}

// ---------------------------------------------------------------------------
// credential lookup
// ---------------------------------------------------------------------------

func TestTokenFromAuthFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if _, err := tokenFromAuthFile(); err == nil || !strings.Contains(err.Error(), "no cline-*.json") {
		t.Fatalf("empty dir: %v", err)
	}
	dir := filepath.Join(home, defaultAuths)
	_ = os.MkdirAll(dir, 0o700)
	_ = os.WriteFile(filepath.Join(dir, "cline-a.json"), []byte("{broken"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "cline-b.json"), []byte(`{"access_token":"  "}`), 0o600)
	if _, err := tokenFromAuthFile(); err == nil || !strings.Contains(err.Error(), "no access_token") {
		t.Fatalf("no usable token: %v", err)
	}
	_ = os.WriteFile(filepath.Join(dir, "cline-c.json"), []byte(`{"access_token":" tok-from-file "}`), 0o600)
	got, err := tokenFromAuthFile()
	if err != nil || got != "tok-from-file" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestRunUsesAuthFileWhenNoToken(t *testing.T) {
	f := newFakeAPI(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	gen := filepath.Join(t.TempDir(), "g.go")
	if code, _, stderr := runTool(t, "--out", gen); code != 1 || !strings.Contains(stderr, "could not read a local Cline credential") {
		t.Fatalf("no credential: exit=%d %s", code, stderr)
	}
	dir := filepath.Join(home, defaultAuths)
	_ = os.MkdirAll(dir, 0o700)
	_ = os.WriteFile(filepath.Join(dir, "cline-x.json"), []byte(`{"access_token":"`+f.token+`"}`), 0o600)
	if code, _, stderr := runTool(t, "--out", gen); code != 0 {
		t.Fatalf("with credential: exit=%d %s", code, stderr)
	}
}

// ---------------------------------------------------------------------------
// resolve / pick
// ---------------------------------------------------------------------------

func rowByID(t *testing.T, rows []row, id string) row {
	t.Helper()
	for _, r := range rows {
		if r.clientID == id {
			return r
		}
	}
	t.Fatalf("row %s not found", id)
	return row{}
}

func TestResolveMatchKinds(t *testing.T) {
	rows := resolve(fixtureFeed(), fixtureCatalog())
	if len(rows) != 5 {
		t.Fatalf("rows=%d", len(rows))
	}
	// tierOrder drives output order.
	if rows[0].tier != "recommended" || rows[len(rows)-1].tier != "clineCloud" {
		t.Fatalf("order: %v", rows)
	}
	cases := []struct {
		id, note, under string
		ctx, out        int64
	}{
		{"vendora/model-x", "vendor", "vendora/model-x", 200000, 64000},         // two vendors, feed names one
		{"cline-free/flash-y:free", "exact", "vendorc/flash-y", 1000000, 65536}, // :free suffix dropped
		{"cline-pass/qwen3.8-max", "override", "qwen/qwen3.8-max-0902", 1000000, 131072},
		{"cline-pass/dated", "prefix", "vendord/dated-2026-09", 400000, 32000}, // unique prefix
		{"cline-cloud/z", "exact", "vendore/z", 50000, 4000},
	}
	for _, c := range cases {
		r := rowByID(t, rows, c.id)
		if r.note != c.note || r.underlying != c.under || r.contextLen != c.ctx || r.maxOutput != c.out || r.warn != "" {
			t.Errorf("%s: got note=%s under=%s ctx=%d out=%d warn=%q", c.id, r.note, r.underlying, r.contextLen, r.maxOutput, r.warn)
		}
	}
	x := rowByID(t, rows, "vendora/model-x")
	if strings.Join(x.inputMods, ",") != "text,image" || strings.Join(x.outputMods, ",") != "text" ||
		strings.Join(x.params, ",") != "max_tokens,tools" {
		t.Errorf("modalities/params: %+v", x)
	}
}

func TestPickMultiCandidateDoesNotGuess(t *testing.T) {
	feed := map[string][]feedEntry{"clinePass": {{ID: "cline-pass/model-x", Name: "X"}}}
	rows := resolve(feed, fixtureCatalog())
	r := rows[0]
	if r.note != "ambiguous" || r.underlying != "" || r.contextLen != 0 {
		t.Fatalf("ambiguous slug must not be guessed: %+v", r)
	}
	if !strings.Contains(r.warn, "ambiguous slug model-x (vendora/model-x, vendorb/model-x)") ||
		!strings.Contains(r.warn, "slugOverrides") {
		t.Fatalf("warn=%q", r.warn)
	}
}

func TestPickAmbiguousPrefixReturnsNil(t *testing.T) {
	catalog := []catalogEntry{cat("a/m-2026-01", 1, 1, "text"), cat("b/m-2026-02", 2, 2, "text")}
	rows := resolve(map[string][]feedEntry{"free": {{ID: "cline-free/m"}}}, catalog)
	if rows[0].underlying != "" || !strings.Contains(rows[0].warn, "no catalog match for slug m") {
		t.Fatalf("two prefix candidates must not be guessed: %+v", rows[0])
	}
}

func TestFreeSuffixAlias(t *testing.T) {
	catalog := []catalogEntry{cat("v/slug", 1234, 56, "text")}
	rows := resolve(map[string][]feedEntry{"free": {{ID: "other/slug-free"}}}, catalog)
	if rows[0].warn != "" || rows[0].underlying != "v/slug" || rows[0].contextLen != 1234 {
		t.Fatalf("%+v", rows[0])
	}
}

func TestOverrideTargetMissingFallsBack(t *testing.T) {
	// The override names qwen/qwen3.8-max-0902; without it the exact row wins.
	catalog := []catalogEntry{cat("qwen/qwen3.8-max", 32000, 8000, "text")}
	rows := resolve(map[string][]feedEntry{"clinePass": {{ID: "cline-pass/qwen3.8-max"}}}, catalog)
	if rows[0].note != "exact" || rows[0].underlying != "qwen/qwen3.8-max" {
		t.Fatalf("%+v", rows[0])
	}
}

func TestPinnedMetaFillsAbsentModel(t *testing.T) {
	feed := map[string][]feedEntry{"free": {{ID: "stealth/pixel-canary", Name: "Pixel"}}}
	rows := resolve(feed, fixtureCatalog())
	r := rows[0]
	pin := pinnedMeta["pixel-canary"]
	if r.note != "pinned" || r.warn != "" || r.contextLen != pin.ContextLength || r.maxOutput != pin.MaxOutput ||
		r.underlying != pin.Underlying || len(r.params) != len(pin.Params) {
		t.Fatalf("pinned: %+v", r)
	}
	// When the catalog does publish it, live data wins over the pin.
	rows = resolve(feed, append(fixtureCatalog(), cat("stealth/pixel-canary", 7, 7, "text")))
	if rows[0].note == "pinned" || rows[0].contextLen != 7 {
		t.Fatalf("catalog must beat pin: %+v", rows[0])
	}
}

func TestResolveWarnings(t *testing.T) {
	noCtx := cat("v/empty", 0, 0, "text")
	noCtx.DescriptionRaw = "  from catalog  "
	feed := map[string][]feedEntry{"free": {{ID: "cline-free/empty"}, {ID: "cline-free/ghost"}}}
	rows := resolve(feed, []catalogEntry{noCtx})
	if rows[0].warn != "catalog row has no context_length" || rows[0].description != "from catalog" {
		t.Fatalf("no ctx: %+v", rows[0])
	}
	if rows[1].warn != "no catalog match for slug ghost" {
		t.Fatalf("ghost: %+v", rows[1])
	}
	// Unknown tiers are ignored, not emitted.
	rows = resolve(map[string][]feedEntry{"mystery": {{ID: "x/y"}}}, nil)
	if len(rows) != 0 {
		t.Fatalf("unknown tier emitted: %+v", rows)
	}
}

func TestRunReportsUnresolvedRows(t *testing.T) {
	f := newFakeAPI(t)
	f.set(func(f *fakeAPI) { f.feed["free"] = append(f.feed["free"], feedEntry{ID: "cline-free/ghost"}) })
	code, _, stderr := runTool(t, "--token", f.token, "--out", filepath.Join(t.TempDir(), "g.go"))
	if code != 0 || !strings.Contains(stderr, "WARN 1 row(s) kept without full metadata") || !strings.Contains(stderr, "cline-free/ghost") {
		t.Fatalf("exit=%d %s", code, stderr)
	}
}

func TestRenderOmitsEmptyFieldsAndFlattensDescription(t *testing.T) {
	src := render([]row{{clientID: "a/b", tier: "free", description: "one\r\ntwo   three"}})
	if !strings.Contains(src, `Description:     "one two three"`) {
		t.Fatalf("description not flattened:\n%s", src)
	}
	for _, absent := range []string{"Underlying", "ContextLength", "MaxOutputTokens", "InputModalities", "Parameters", "DisplayName"} {
		if strings.Contains(src, absent+":") {
			t.Fatalf("%s should be omitted", absent)
		}
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func TestHelpers(t *testing.T) {
	if leafOf("a/b/c") != "c" || leafOf("c") != "c" || vendorOf("a/b") != "a" || vendorOf("b") != "" {
		t.Fatal("leafOf/vendorOf")
	}
	if int64Field(nil, "k") != 0 || int64Field(map[string]any{"k": int64(3)}, "k") != 3 ||
		int64Field(map[string]any{"k": 4}, "k") != 4 || int64Field(map[string]any{"k": "5"}, "k") != 0 {
		t.Fatal("int64Field")
	}
	if stringSliceField(nil, "k") != nil || stringSliceField(map[string]any{"k": "x"}, "k") != nil ||
		stringSliceField(map[string]any{"k": []any{"", 1}}, "k") != nil {
		t.Fatal("stringSliceField empty cases")
	}
	if truncate("abc", 5) != "abc" || truncate("abcdef", 3) != "abc…" {
		t.Fatal("truncate")
	}
	if goSlice([]string{"a", "b"}) != `[]string{"a", "b"}` {
		t.Fatal("goSlice")
	}
	if !sameIgnoringTimestamp("// Refreshed: 1\nx", "// Refreshed: 2\nx") || sameIgnoringTimestamp("x", "y") {
		t.Fatal("sameIgnoringTimestamp")
	}
}

// TestIdentityMatchesPlugin keeps the tool's API identity on the same versions
// the plugin sends (plugin/cline_headers.go), so build.sh's version check also
// covers this tool.
func TestIdentityMatchesPlugin(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "plugin", "cline_headers.go"))
	if err != nil {
		t.Skip("plugin source not present:", err)
	}
	get := func(name string) string {
		m := regexp.MustCompile(`(?m)^\s*` + name + `\s*=\s*"([^"]*)"`).FindSubmatch(src)
		if m == nil {
			t.Fatalf("%s not found in plugin/cline_headers.go", name)
		}
		return string(m[1])
	}
	if v := get("defaultCLIClientVersion"); v != clientVersion {
		t.Errorf("modelmeta clientVersion=%s, plugin defaultCLIClientVersion=%s", clientVersion, v)
	}
	if v := get("defaultCoreVersion"); v != coreVersion {
		t.Errorf("modelmeta coreVersion=%s, plugin defaultCoreVersion=%s", coreVersion, v)
	}
}
