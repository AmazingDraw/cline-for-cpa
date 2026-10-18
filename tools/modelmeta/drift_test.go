package main

import (
	"go/format"
	"strings"
	"testing"
)

func baseRows() []row {
	return []row{
		{clientID: "cline-pass/a", tier: "clinePass", displayName: "A", underlying: "v/a", contextLen: 1000, maxOutput: 100,
			inputMods: []string{"text", "image"}, outputMods: []string{"text"}, params: []string{"max_tokens", "tools"}},
		{clientID: "cline-free/b", tier: "free", underlying: "v/b", contextLen: 2000, maxOutput: 200,
			inputMods: []string{"text"}, outputMods: []string{"text"}, params: []string{"tools"}},
		{clientID: "stealth/c", tier: "free", underlying: "stealth/c", contextLen: 3000, maxOutput: 300},
	}
}

// committed renders rows the same way the tool does and parses them back,
// so the tests also cover the parseGenerated round-trip.
func committed(t *testing.T, rows []row) genCatalog {
	t.Helper()
	src, err := format.Source([]byte(render(rows)))
	if err != nil {
		t.Fatal(err)
	}
	c, err := parseGenerated(string(src))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestParseRoundTripClean(t *testing.T) {
	d := compareCatalogs(committed(t, baseRows()), rowsToCatalog(baseRows()))
	if !d.clean() {
		t.Fatalf("expected no drift, got\n%s", formatDrift(d))
	}
}

func TestOrderOnlyIsWarn(t *testing.T) {
	cur := baseRows()
	cur[0], cur[1] = cur[1], cur[0]
	cur[1].inputMods = []string{"image", "text"} // cur[1] is now cline-pass/a
	d := compareCatalogs(committed(t, baseRows()), rowsToCatalog(cur))
	if len(d.hard) != 0 || len(d.warn) != 2 {
		t.Fatalf("want 0 hard / 2 warn, got\n%s", formatDrift(d))
	}
}

func TestNumericOnlyIsWarn(t *testing.T) {
	cur := baseRows()
	cur[0].maxOutput = 128
	cur[1].contextLen = 1
	cur[2].displayName = "C"
	d := compareCatalogs(committed(t, baseRows()), rowsToCatalog(cur))
	if len(d.hard) != 0 || len(d.warn) != 3 {
		t.Fatalf("want 0 hard / 3 warn, got\n%s", formatDrift(d))
	}
}

func TestAddRemoveIsHard(t *testing.T) {
	cur := baseRows()[:2]
	cur = append(cur, row{clientID: "cline-pass/new", tier: "clinePass", underlying: "v/new"})
	d := compareCatalogs(committed(t, baseRows()), rowsToCatalog(cur))
	if len(d.hard) != 2 || !strings.Contains(formatDrift(d), "model added: cline-pass/new") ||
		!strings.Contains(formatDrift(d), "model removed: stealth/c") {
		t.Fatalf("want add+remove hard, got\n%s", formatDrift(d))
	}
}

func TestRoutingFieldsAreHard(t *testing.T) {
	cur := baseRows()
	cur[0].tier = "clineCloud"
	cur[1].underlying = "v/other"
	cur[2].params = []string{"tools"}
	d := compareCatalogs(committed(t, baseRows()), rowsToCatalog(cur))
	if len(d.hard) != 3 {
		t.Fatalf("want 3 hard, got\n%s", formatDrift(d))
	}
}
