package plugin

import (
	"encoding/json"
	"testing"
)

// The upstream rejects unknown reasoning_effort values with a silent empty
// response (verified A/B 2026-09-23), so only legal values may pass through.
func TestNormalizeReasoningEffort(t *testing.T) {
	cases := []struct {
		name       string
		input      any
		present    bool
		wantValue  any
		wantAbsent bool
		wantAction string
	}{
		{name: "valid low untouched", input: "low", present: true, wantValue: "low"},
		{name: "valid xhigh untouched", input: "xhigh", present: true, wantValue: "xhigh"},
		{name: "valid none untouched", input: "none", present: true, wantValue: "none"},
		{name: "uppercase repaired", input: "HIGH", present: true, wantValue: "high", wantAction: "repaired"},
		{name: "padded repaired", input: " high ", present: true, wantValue: "high", wantAction: "repaired"},
		{name: "unknown dropped", input: "bogus", present: true, wantAbsent: true, wantAction: "dropped"},
		{name: "empty dropped", input: "", present: true, wantAbsent: true, wantAction: "dropped"},
		{name: "whitespace only dropped", input: "   ", present: true, wantAbsent: true, wantAction: "dropped"},
		{name: "non-string dropped", input: 5, present: true, wantAbsent: true, wantAction: "dropped"},
		{name: "boolean dropped", input: true, present: true, wantAbsent: true, wantAction: "dropped"},
		{name: "absent untouched", present: false},
	}
	for _, tc := range cases {
		obj := map[string]any{"model": "cline-pass/x"}
		if tc.present {
			obj["reasoning_effort"] = tc.input
		}
		action, _ := normalizeReasoningEffortInPayload(obj, pluginConfig{})
		if action != tc.wantAction {
			t.Fatalf("%s: action = %q, want %q", tc.name, action, tc.wantAction)
		}
		got, ok := obj["reasoning_effort"]
		if tc.wantAbsent {
			if ok {
				t.Fatalf("%s: field should be removed, got %v", tc.name, got)
			}
			continue
		}
		if !tc.present {
			if ok {
				t.Fatalf("%s: field must stay absent", tc.name)
			}
			continue
		}
		if !ok || got != tc.wantValue {
			t.Fatalf("%s: value = %v, want %v", tc.name, got, tc.wantValue)
		}
	}
}

func TestNormalizeReasoningEffortCanBeDisabled(t *testing.T) {
	off := false
	cfg := pluginConfig{ReasoningEffortNormalize: &off}
	obj := map[string]any{"reasoning_effort": "bogus"}
	if action, _ := normalizeReasoningEffortInPayload(obj, cfg); action != "" {
		t.Fatalf("action = %q, want none when normalization is disabled", action)
	}
	if obj["reasoning_effort"] != "bogus" {
		t.Fatal("disabled normalization must not touch the payload")
	}
}

// The payload actually sent upstream must carry only legal values.
func TestRewriteModelFiltersReasoningEffort(t *testing.T) {
	payload := []byte(`{"model":"whatever","reasoning_effort":"HIGH","messages":[]}`)
	out, err := rewriteModel(pluginConfig{AuthDir: t.TempDir()}, payload, "cline-pass/deepseek-v4.1-flash")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["model"] != "cline-pass/deepseek-v4.1-flash" {
		t.Fatalf("model = %v", got["model"])
	}
	if got["reasoning_effort"] != "high" {
		t.Fatalf("reasoning_effort = %v, want repaired lowercase", got["reasoning_effort"])
	}

	payload2 := []byte(`{"model":"whatever","reasoning_effort":"turbo","messages":[]}`)
	out2, err := rewriteModel(pluginConfig{AuthDir: t.TempDir()}, payload2, "cline-pass/kimi-k3")
	if err != nil {
		t.Fatal(err)
	}
	var got2 map[string]any
	_ = json.Unmarshal(out2, &got2)
	if _, present := got2["reasoning_effort"]; present {
		t.Fatalf("illegal value must be dropped before reaching upstream: %v", got2["reasoning_effort"])
	}
}
