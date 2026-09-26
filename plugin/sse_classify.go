package plugin

import (
	"bytes"
	"encoding/json"
	"strings"
)

// ClassifySSEData decides whether an upstream SSE data payload is liveness-only.
//
// Liveness-only (does NOT refresh lastMeaningful):
//   - empty / whitespace
//   - SSE ping / heartbeat markers
//   - OpenAI chunks whose choices[].delta is empty (no content, reasoning, tool_calls)
//
// Meaningful: any content / reasoning_content / tool_calls delta, or non-empty text.
//
// Plugin-injected keepalives must never reach NoteInbound at all — this helper
// is only for real upstream bytes.
func ClassifySSEData(data []byte) (livenessOnly bool) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("[DONE]")) {
		return true
	}
	lower := strings.ToLower(string(trimmed))
	if lower == "ping" || lower == "heartbeat" || lower == ": ping" {
		return true
	}
	var payload struct {
		Choices []struct {
			Delta struct {
				Content          any `json:"content"`
				Reasoning        any `json:"reasoning"`
				ReasoningContent any `json:"reasoning_content"`
				ToolCalls        any `json:"tool_calls"`
				Role             any `json:"role"`
			} `json:"delta"`
			FinishReason any `json:"finish_reason"`
		} `json:"choices"`
		Error any `json:"error"`
	}
	if err := json.Unmarshal(trimmed, &payload); err != nil {
		// Non-JSON upstream frames still count as inbound activity; treat as
		// meaningful so we don't heartbeat-only-fail on unknown formats.
		return false
	}
	if payload.Error != nil {
		return false
	}
	if len(payload.Choices) == 0 {
		return true
	}
	for _, ch := range payload.Choices {
		if ch.FinishReason != nil && ch.FinishReason != "" {
			return false
		}
		if nonemptyJSON(ch.Delta.Content) || nonemptyJSON(ch.Delta.Reasoning) || nonemptyJSON(ch.Delta.ReasoningContent) || nonemptyJSON(ch.Delta.ToolCalls) {
			return false
		}
	}
	return true
}

func nonemptyJSON(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(x) != ""
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	default:
		return true
	}
}

// IsSSEComment reports SSE comment/ping lines (": ..." or "event: ping").
func IsSSEComment(line []byte) bool {
	s := bytes.TrimSpace(line)
	if len(s) == 0 {
		return true
	}
	if s[0] == ':' {
		return true
	}
	lower := strings.ToLower(string(s))
	return strings.HasPrefix(lower, "event: ping") || strings.HasPrefix(lower, "event:heartbeat")
}
