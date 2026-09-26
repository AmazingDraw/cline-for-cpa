package plugin

import "testing"

func TestNormalizeModelClinePassUpstream(t *testing.T) {
	cases := map[string]string{
		"cline-pass/deepseek-v4.1-flash": "cline-pass/deepseek-v4.1-flash",
		"deepseek-v4.1-flash":            "cline-pass/deepseek-v4.1-flash",
		"deepseek/deepseek-v4.1-flash":   "cline-pass/deepseek-v4.1-flash",
		"cline-pass/kimi-k3":             "cline-pass/kimi-k3",
		"kimi-k3":                        "cline-pass/kimi-k3",
		"stealth/space-bunny-alpha":      "stealth/space-bunny-alpha",
	}
	for in, want := range cases {
		client, up, err := NormalizeModel(in)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if up != want || client != want {
			t.Fatalf("%s: client=%q upstream=%q want %q", in, client, up, want)
		}
	}
}

func TestStaticModelIDsNoAliases(t *testing.T) {
	ids := StaticModelIDs()
	for _, id := range ids {
		if id == "cline-pass/cline-ds-flash" || id == "cline-pass/mimo-flash" || id == "cline-pass/mimo-pro" {
			t.Fatalf("alias leaked: %s", id)
		}
	}
	want := map[string]bool{
		"cline-pass/deepseek-v4.1-flash": false,
		"cline-pass/mimo-v2.6-flash":     false,
		"cline-pass/mimo-v2.6-pro":       false,
		"cline-pass/kimi-k3":             false,
	}
	for _, id := range ids {
		if _, ok := want[id]; ok {
			want[id] = true
		}
	}
	for id, seen := range want {
		if !seen {
			t.Fatalf("missing %s", id)
		}
	}
}
