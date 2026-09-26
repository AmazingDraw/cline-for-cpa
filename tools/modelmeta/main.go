// Command modelmeta refreshes the Cline model catalog embedded in the plugin.
//
// Cline publishes two views we need:
//
//	GET /api/v1/ai/cline/recommended-models  → the tiers a client may pick
//	                                           (recommended / free / clinePass / clineCloud)
//	GET /api/v1/ai/cline/models              → ~450 OpenRouter-style catalog rows
//	                                           carrying context_length, capabilities,
//	                                           modalities, supported_parameters
//
// The feed names models after *our* namespaces (cline-pass/x, cline-free/x) while
// the catalog names the underlying vendor model (deepseek/deepseek-v4.1-flash),
// so the two are joined on the model slug. Everything is written to a generated
// Go file; nothing is fetched at plugin runtime.
//
// Usage:
//
//	go run ./tools/modelmeta           # fetch and rewrite plugin/modelmeta_gen.go
//	go run ./tools/modelmeta --check   # report drift without writing (exit 1 on drift)
//	go run ./tools/modelmeta --token sk_…   # bypass the local auth file
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/format"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	baseURL       = "https://api.cline.bot/api/v1"
	feedPath      = "/ai/cline/recommended-models"
	catalogPath   = "/ai/cline/models"
	outFile       = "plugin/modelmeta_gen.go"
	defaultAuths  = ".cli-proxy-api/auths"
	requestTimout = 45 * time.Second
	userAgent     = "Cline/3.0.64"
)

// slugOverrides pins a feed leaf to an exact catalog id when neither an exact
// nor a unique prefix match is good enough. Keys are feed leaves, values are
// "vendor/slug" catalog ids.
var slugOverrides = map[string]string{
	"qwen3.8-max": "qwen/qwen3.8-max-0902",
}

// pinnedMeta keeps hand-verified metadata for feed rows that the catalog does
// not publish at all, so a routine refresh cannot silently strip a model down
// to a name and a description.
//
// The generated file is overwritten by every `go run ./tools/modelmeta`, so
// editing plugin/modelmeta_gen.go by hand only survives until the next refresh.
// stealth/pixel-canary is exactly that case: it is absent from the catalog, and
// a refresh used to drop its 1M context / 128K output / modality list — the
// values a client needs to size its request correctly.
var pinnedMeta = map[string]struct {
	ContextLength int64
	MaxOutput     int64
	InputMods     []string
	OutputMods    []string
	Params        []string
	Underlying    string
}{
	"pixel-canary": {
		ContextLength: 1048576,
		MaxOutput:     131072,
		InputMods:     []string{"text", "image"},
		OutputMods:    []string{"text"},
		Params: []string{
			"include_reasoning", "max_tokens", "reasoning", "reasoning_effort",
			"response_format", "temperature", "tool_choice", "tools", "top_p",
		},
		Underlying: "stealth/pixel-canary",
	},
}

// tierOrder keeps the generated file (and therefore the model list) stable.
var tierOrder = []string{"recommended", "free", "clinePass", "clineCloud"}

type feedEntry struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

type catalogEntry struct {
	ID             string            `json:"id"`
	CanonicalSlug  string            `json:"canonical_slug"`
	Name           string            `json:"name"`
	ContextLength  int64             `json:"context_length"`
	Architecture   map[string]any    `json:"architecture"`
	Supported      []string          `json:"supported_parameters"`
	Pricing        map[string]string `json:"pricing"`
	TopProvider    map[string]any    `json:"top_provider"`
	PerRequestLim  map[string]any    `json:"per_request_limits"`
	DescriptionRaw string            `json:"description"`
}

type row struct {
	clientID    string
	tier        string
	displayName string
	description string
	underlying  string
	contextLen  int64
	maxOutput   int64
	inputMods   []string
	outputMods  []string
	params      []string
	// warnings collected while resolving this row
	warn string
	// note records how the catalog row was matched (exact / prefix / override)
	note string
}

func main() {
	var (
		check   = flag.Bool("check", false, "report drift without writing the file")
		token   = flag.String("token", "", "bearer to use instead of the local auth file")
		out     = flag.String("out", outFile, "output path for the generated Go file")
		verbose = flag.Bool("v", false, "print every resolved row")
	)
	flag.Parse()

	bearer := strings.TrimSpace(*token)
	if bearer == "" {
		var err error
		bearer, err = tokenFromAuthFile()
		if err != nil {
			fatal("no --token given and could not read a local Cline credential: %v", err)
		}
	}

	feed, err := fetchFeed(bearer)
	if err != nil {
		fatal("fetch feed: %v", err)
	}
	catalog, err := fetchCatalog(bearer)
	if err != nil {
		fatal("fetch catalog: %v", err)
	}

	rows := resolve(feed, catalog)
	generated := render(rows)
	// Run the generated source through gofmt so a refresh never shows up as a
	// formatting-only diff.
	formatted, errFormat := format.Source([]byte(generated))
	if errFormat != nil {
		fatal("generated source does not compile-parse: %v", errFormat)
	}
	generated = string(formatted)

	if *verbose {
		for _, r := range rows {
			fmt.Printf("%-46s %-10s ctx=%-9d out=%-8d %-9s under=%-34s %s\n",
				r.clientID, r.tier, r.contextLen, r.maxOutput, r.note, r.underlying, r.warn)
		}
	}

	// Report what could not be resolved instead of silently shipping blanks.
	var missing []string
	for _, r := range rows {
		if r.warn != "" {
			missing = append(missing, r.clientID+" ("+r.warn+")")
		}
	}
	if len(missing) > 0 {
		fmt.Fprintf(os.Stderr, "WARN %d row(s) kept without full metadata:\n", len(missing))
		for _, m := range missing {
			fmt.Fprintln(os.Stderr, "  -", m)
		}
	}

	if *check {
		old, _ := os.ReadFile(*out)
		if sameIgnoringTimestamp(string(old), generated) {
			fmt.Printf("modelmeta: up to date (%d rows)\n", len(rows))
			return
		}
		fmt.Fprintf(os.Stderr, "modelmeta: %s is stale — run `go run ./tools/modelmeta`\n", *out)
		os.Exit(1)
	}

	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		fatal("mkdir: %v", err)
	}
	if err := os.WriteFile(*out, []byte(generated), 0o644); err != nil {
		fatal("write %s: %v", *out, err)
	}
	fmt.Printf("modelmeta: wrote %s (%d rows)\n", *out, len(rows))
}

// tokenFromAuthFile reads the single cline-*.json credential in the default
// auth dir. It never mutates the file.
func tokenFromAuthFile() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, defaultAuths)
	matches, _ := filepath.Glob(filepath.Join(dir, "cline-*.json"))
	if len(matches) == 0 {
		return "", fmt.Errorf("no cline-*.json in %s", dir)
	}
	for _, m := range matches {
		raw, err := os.ReadFile(m)
		if err != nil {
			continue
		}
		var doc struct {
			AccessToken string `json:"access_token"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			continue
		}
		if t := strings.TrimSpace(doc.AccessToken); t != "" {
			return t, nil
		}
	}
	return "", errors.New("no access_token in cline credential files")
}

func get(bearer, path string, out any) error {
	req, err := http.NewRequest(http.MethodGet, baseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("X-CLIENT-TYPE", "cline-cli")
	req.Header.Set("X-CLIENT-VERSION", "3.0.64")
	req.Header.Set("X-PLATFORM", "cli")
	req.Header.Set("X-PLATFORM-VERSION", "3.0.64")
	req.Header.Set("X-CORE-VERSION", "3.0.64")
	req.Header.Set("HTTP-Referer", "https://cline.bot")
	req.Header.Set("X-Title", "Cline")

	client := &http.Client{Timeout: requestTimout}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	return json.Unmarshal(body, out)
}

func fetchFeed(bearer string) (map[string][]feedEntry, error) {
	var raw map[string][]feedEntry
	if err := get(bearer, feedPath, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

func fetchCatalog(bearer string) ([]catalogEntry, error) {
	var raw struct {
		Data []catalogEntry `json:"data"`
	}
	if err := get(bearer, catalogPath, &raw); err != nil {
		return nil, err
	}
	return raw.Data, nil
}

// resolve joins feed entries onto catalog rows by model slug.
func resolve(feed map[string][]feedEntry, catalog []catalogEntry) []row {
	byLeaf := map[string][]catalogEntry{}
	for _, c := range catalog {
		leaf := leafOf(c.ID)
		byLeaf[leaf] = append(byLeaf[leaf], c)
	}

	var rows []row
	for _, tier := range tierOrder {
		for _, e := range feed[tier] {
			leaf := leafOf(strings.TrimSuffix(e.ID, ":free"))
			r := row{
				clientID:    e.ID,
				tier:        tier,
				displayName: strings.TrimSpace(e.Name),
				description: strings.TrimSpace(e.Description),
			}
			match := pick(leaf, e.ID, byLeaf, &r.note)
			if match == nil {
				// No catalog row: fall back to the hand-verified pin when we have
				// one, so a model that is simply absent from the catalog keeps
				// advertising usable limits instead of degrading to name-only.
				if pin, ok := pinnedMeta[leaf]; ok {
					r.note = "pinned"
					r.underlying = pin.Underlying
					r.contextLen = pin.ContextLength
					r.maxOutput = pin.MaxOutput
					r.inputMods = pin.InputMods
					r.outputMods = pin.OutputMods
					r.params = pin.Params
					rows = append(rows, r)
					continue
				}
				r.warn = "no catalog match for slug " + leaf
				rows = append(rows, r)
				continue
			}
			r.underlying = match.ID
			r.contextLen = match.ContextLength
			r.maxOutput = int64Field(match.TopProvider, "max_completion_tokens")
			r.inputMods = stringSliceField(match.Architecture, "input_modalities")
			r.outputMods = stringSliceField(match.Architecture, "output_modalities")
			r.params = match.Supported
			if r.description == "" {
				r.description = strings.TrimSpace(match.DescriptionRaw)
			}
			if r.contextLen == 0 {
				r.warn = "catalog row has no context_length"
			}
			rows = append(rows, r)
		}
	}
	return rows
}

// pick chooses the catalog row for a slug, in order of confidence: explicit
// override → exact match → unique prefix match. When several vendors publish the
// same slug the override table is the only way to decide, so an ambiguous lookup
// returns nil and surfaces as a warning instead of a wrong context window.
func pick(leaf, feedID string, byLeaf map[string][]catalogEntry, note *string) *catalogEntry {
	if want := slugOverrides[leaf]; want != "" {
		for i := range byLeaf[leafOf(want)] {
			if byLeaf[leafOf(want)][i].ID == want {
				*note = "override"
				return &byLeaf[leafOf(want)][i]
			}
		}
	}
	cands := byLeaf[leaf]
	if len(cands) == 0 {
		// poolside/laguna-s-2.1:free and friends: the catalog drops the :free suffix.
		cands = byLeaf[strings.TrimSuffix(leaf, ":free")]
	}
	if len(cands) == 0 {
		// Catalog slugs often carry a date suffix (qwen3.8-max → qwen3.8-max-0902).
		// Only a *unique* prefix match is trusted.
		var pref []catalogEntry
		for key, list := range byLeaf {
			if key != leaf && strings.HasPrefix(key, leaf) {
				pref = append(pref, list...)
			}
		}
		if len(pref) == 1 {
			*note = "prefix"
			return &pref[0]
		}
		return nil
	}
	// Prefer the vendor the feed itself names, when it names one.
	vendor := vendorOf(feedID)
	for i := range cands {
		if vendor != "" && strings.HasPrefix(cands[i].ID, vendor+"/") {
			*note = "vendor"
			return &cands[i]
		}
	}
	if len(cands) == 1 {
		*note = "exact"
		return &cands[0]
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].ID < cands[j].ID })
	*note = "ambiguous-sorted"
	return &cands[0]
}

func render(rows []row) string {
	var b strings.Builder
	b.WriteString("// Code generated by tools/modelmeta. DO NOT EDIT.\n")
	b.WriteString("//\n")
	b.WriteString("// Refreshed: " + time.Now().Format(time.RFC3339) + "\n")
	b.WriteString("// Sources:\n")
	b.WriteString("//   " + baseURL + feedPath + "\n")
	b.WriteString("//   " + baseURL + catalogPath + "\n")
	b.WriteString("//\n")
	b.WriteString("// Refresh: go run ./tools/modelmeta\n")
	b.WriteString("// Verify:  go run ./tools/modelmeta --check\n\n")
	b.WriteString("package plugin\n\n")
	b.WriteString("// generatedCatalog carries display metadata for every id the Cline feed\n")
	b.WriteString("// advertises, keyed by the client-facing id we publish.\n")
	b.WriteString("var generatedCatalog = map[string]modelMeta{\n")
	for _, r := range rows {
		b.WriteString(fmt.Sprintf("\t%q: {\n", r.clientID))
		b.WriteString(fmt.Sprintf("\t\tTier:            %q,\n", r.tier))
		if r.displayName != "" {
			b.WriteString(fmt.Sprintf("\t\tDisplayName:     %q,\n", r.displayName))
		}
		if r.description != "" {
			b.WriteString(fmt.Sprintf("\t\tDescription:     %q,\n", oneLine(r.description)))
		}
		if r.underlying != "" {
			b.WriteString(fmt.Sprintf("\t\tUnderlying:      %q,\n", r.underlying))
		}
		if r.contextLen > 0 {
			b.WriteString(fmt.Sprintf("\t\tContextLength:   %d,\n", r.contextLen))
		}
		if r.maxOutput > 0 {
			b.WriteString(fmt.Sprintf("\t\tMaxOutputTokens: %d,\n", r.maxOutput))
		}
		if len(r.inputMods) > 0 {
			b.WriteString(fmt.Sprintf("\t\tInputModalities: %s,\n", goSlice(r.inputMods)))
		}
		if len(r.outputMods) > 0 {
			b.WriteString(fmt.Sprintf("\t\tOutputModalities: %s,\n", goSlice(r.outputMods)))
		}
		if len(r.params) > 0 {
			b.WriteString(fmt.Sprintf("\t\tParameters:      %s,\n", goSlice(r.params)))
		}
		b.WriteString("\t},\n")
	}
	b.WriteString("}\n\n")
	b.WriteString("// generatedOrder preserves the feed's tier/order for a stable model list.\n")
	b.WriteString("var generatedOrder = []string{\n")
	for _, r := range rows {
		b.WriteString(fmt.Sprintf("\t%q,\n", r.clientID))
	}
	b.WriteString("}\n")
	return b.String()
}

// refreshedLine matches the volatile header stamp. It is the only part of the
// generated file that changes between two fetches of identical data, so it is
// stripped before --check compares — otherwise every run would look like drift.
var refreshedLine = regexp.MustCompile(`(?m)^// Refreshed: .*\n`)

func sameIgnoringTimestamp(a, b string) bool {
	return refreshedLine.ReplaceAllString(a, "") == refreshedLine.ReplaceAllString(b, "")
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	return strings.TrimSpace(s)
}

func goSlice(vals []string) string {
	parts := make([]string, 0, len(vals))
	for _, v := range vals {
		parts = append(parts, fmt.Sprintf("%q", v))
	}
	return "[]string{" + strings.Join(parts, ", ") + "}"
}

func leafOf(id string) string {
	if i := strings.LastIndex(id, "/"); i >= 0 {
		return id[i+1:]
	}
	return id
}

func vendorOf(id string) string {
	if i := strings.Index(id, "/"); i >= 0 {
		return id[:i]
	}
	return ""
}

func int64Field(m map[string]any, key string) int64 {
	if m == nil {
		return 0
	}
	switch v := m[key].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	}
	return 0
}

func stringSliceField(m map[string]any, key string) []string {
	if m == nil {
		return nil
	}
	raw, ok := m[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if s, ok := item.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "modelmeta: "+format+"\n", args...)
	os.Exit(1)
}
