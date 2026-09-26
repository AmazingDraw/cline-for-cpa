package plugin

import "testing"

func TestClassifySSEData(t *testing.T) {
	cases := []struct {
		name string
		data string
		live bool
	}{
		{"done", "[DONE]", true},
		{"empty", "  ", true},
		{"ping", "ping", true},
		{"empty delta", `{"id":"1","choices":[{"delta":{},"index":0}]}`, true},
		{"role only", `{"choices":[{"delta":{"role":"assistant"}}]}`, true},
		{"content", `{"choices":[{"delta":{"content":"hi"}}]}`, false},
		{"reasoning", `{"choices":[{"delta":{"reasoning_content":"..."}}]}`, false},
		{"tools", `{"choices":[{"delta":{"tool_calls":[{"id":"c"}]}}]}`, false},
		{"finish", `{"choices":[{"delta":{},"finish_reason":"stop"}]}`, false},
		{"error", `{"error":{"message":"boom"}}`, false},
	}
	for _, tc := range cases {
		if got := ClassifySSEData([]byte(tc.data)); got != tc.live {
			t.Fatalf("%s: got livenessOnly=%v want %v", tc.name, got, tc.live)
		}
	}
}

func TestIsSSEComment(t *testing.T) {
	if !IsSSEComment([]byte(": keepalive")) {
		t.Fatal("comment")
	}
	if IsSSEComment([]byte("data: {\"a\":1}")) {
		t.Fatal("data line is not comment")
	}
}
