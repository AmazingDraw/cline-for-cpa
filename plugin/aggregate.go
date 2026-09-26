package plugin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Cline's upstream rejects non-streaming chat completions outright with
// {"error":"empty response content","success":false} — verified against the same
// key/model where the streaming request succeeds. The plugin therefore always
// talks to the upstream in streaming mode and, for non-streaming clients,
// aggregates the SSE deltas back into a single chat.completion object here.

// toolCallAccumulator merges streamed tool_call fragments by index.
type toolCallAccumulator struct {
	id   string
	typ  string
	name string
	args strings.Builder
}

// completionAggregate folds OpenAI-style chat.completion.chunk payloads into one
// non-streaming chat.completion response.
type completionAggregate struct {
	id               string
	created          int64
	model            string
	systemFP         string
	content          strings.Builder
	reasoning        strings.Builder
	reasoningDetails json.RawMessage
	finishReason     string
	midStreamErr     *midStreamErrorDetail
	usage            json.RawMessage
	toolIndexes      []int
	toolCalls        map[int]*toolCallAccumulator
}

type midStreamErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func newCompletionAggregate() *completionAggregate {
	return &completionAggregate{toolCalls: map[int]*toolCallAccumulator{}}
}

type chunkEnvelope struct {
	ID                string `json:"id"`
	Created           int64  `json:"created"`
	Model             string `json:"model"`
	SystemFingerprint string `json:"system_fingerprint"`
	Usage             json.RawMessage
	Choices           []struct {
		FinishReason *string               `json:"finish_reason"`
		Error        *midStreamErrorDetail `json:"error,omitempty"`
		Delta        struct {
			Role             string          `json:"role"`
			Content          string          `json:"content"`
			Reasoning        string          `json:"reasoning"`
			ReasoningContent string          `json:"reasoning_content"`
			ReasoningDetails json.RawMessage `json:"reasoning_details,omitempty"`
			ToolCalls        []struct {
				Index    *int   `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
}

// Consume folds one SSE data payload. "[DONE]" terminates the stream.
func (a *completionAggregate) Consume(payload []byte) error {
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 {
		return nil
	}
	if bytes.Equal(payload, []byte("[DONE]")) {
		return nil
	}
	var chunk chunkEnvelope
	if err := json.Unmarshal(payload, &chunk); err != nil {
		// Unknown framing (comments, keepalives) must not abort the aggregate.
		return nil
	}
	if chunk.ID != "" {
		a.id = chunk.ID
	}
	if chunk.Created != 0 {
		a.created = chunk.Created
	}
	if chunk.Model != "" {
		a.model = chunk.Model
	}
	if chunk.SystemFingerprint != "" {
		a.systemFP = chunk.SystemFingerprint
	}
	if len(chunk.Usage) > 0 && !bytes.Equal(bytes.TrimSpace(chunk.Usage), []byte("null")) {
		a.usage = chunk.Usage
	}
	for _, choice := range chunk.Choices {
		if choice.FinishReason != nil && *choice.FinishReason != "" {
			a.finishReason = *choice.FinishReason
			if *choice.FinishReason == "error" && choice.Error != nil {
				a.midStreamErr = choice.Error
			}
		}
		a.content.WriteString(choice.Delta.Content)
		a.reasoning.WriteString(choice.Delta.Reasoning)
		a.reasoning.WriteString(choice.Delta.ReasoningContent)
		if len(choice.Delta.ReasoningDetails) > 0 && !bytes.Equal(bytes.TrimSpace(choice.Delta.ReasoningDetails), []byte("null")) {
			a.reasoningDetails = choice.Delta.ReasoningDetails
		}
		for _, tc := range choice.Delta.ToolCalls {
			idx := 0
			if tc.Index != nil {
				idx = *tc.Index
			}
			acc, ok := a.toolCalls[idx]
			if !ok {
				acc = &toolCallAccumulator{}
				a.toolCalls[idx] = acc
				a.toolIndexes = append(a.toolIndexes, idx)
			}
			if tc.ID != "" {
				acc.id = tc.ID
			}
			if tc.Type != "" {
				acc.typ = tc.Type
			}
			if tc.Function.Name != "" {
				acc.name = tc.Function.Name
			}
			acc.args.WriteString(tc.Function.Arguments)
		}
	}
	return nil
}

// Result renders the aggregated chat.completion document.
func (a *completionAggregate) Result() ([]byte, error) {
	if a.finishReason == "error" && a.midStreamErr != nil {
		code := a.midStreamErr.Code
		if code == "" {
			code = "mid_stream_error"
		}
		msg := a.midStreamErr.Message
		if msg == "" {
			msg = "upstream error during generation"
		}
		return nil, fmt.Errorf("upstream mid-stream error (%s): %s", code, msg)
	}

	content := a.content.String()
	reasoning := a.reasoning.String()
	if content == "" && reasoning == "" && len(a.toolCalls) == 0 {
		return nil, fmt.Errorf("upstream returned empty content")
	}
	message := map[string]any{"role": "assistant", "content": content}
	if reasoning != "" {
		// Clients differ on the key; emit both spellings for compatibility.
		message["reasoning_content"] = reasoning
		message["reasoning"] = reasoning
	}
	if len(a.reasoningDetails) > 0 {
		var details any
		if json.Unmarshal(a.reasoningDetails, &details) == nil {
			message["reasoning_details"] = details
		}
	}
	if len(a.toolCalls) > 0 {
		calls := make([]map[string]any, 0, len(a.toolIndexes))
		for _, idx := range a.toolIndexes {
			acc := a.toolCalls[idx]
			calls = append(calls, map[string]any{
				"id":   acc.id,
				"type": firstNonEmpty(acc.typ, "function"),
				"function": map[string]any{
					"name":      acc.name,
					"arguments": acc.args.String(),
				},
			})
		}
		message["tool_calls"] = calls
	}
	finish := a.finishReason
	if finish == "" {
		if len(a.toolCalls) > 0 {
			finish = "tool_calls"
		} else {
			finish = "stop"
		}
	}
	out := map[string]any{
		"object": "chat.completion",
		"choices": []map[string]any{{
			"index":         0,
			"message":       message,
			"finish_reason": finish,
		}},
	}
	if a.id != "" {
		out["id"] = a.id
	}
	if a.created != 0 {
		out["created"] = a.created
	}
	if a.model != "" {
		out["model"] = a.model
	}
	if a.systemFP != "" {
		out["system_fingerprint"] = a.systemFP
	}
	if len(a.usage) > 0 {
		out["usage"] = normalizeUsagePayload(a.usage)
	}
	return json.Marshal(out)
}

func normalizeUsagePayload(raw json.RawMessage) any {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return raw
	}
	// Ensure prompt_tokens_details is present with cached_tokens if available
	ptd, hasPtd := m["prompt_tokens_details"].(map[string]any)
	if !hasPtd || ptd == nil {
		ptd = map[string]any{
			"cached_tokens": 0,
		}
		m["prompt_tokens_details"] = ptd
	} else if _, ok := ptd["cached_tokens"]; !ok {
		ptd["cached_tokens"] = 0
	}
	return m
}

// aggregateSSEPayloads is the pure core used by tests and by the executor.
func aggregateSSEPayloads(payloads [][]byte) ([]byte, error) {
	agg := newCompletionAggregate()
	for _, p := range payloads {
		if err := agg.Consume(p); err != nil {
			return nil, err
		}
	}
	return agg.Result()
}

// aggregateChunks folds the chunk envelopes produced by collectStreamOnce.
func aggregateChunks(chunks []map[string]any) ([]byte, error) {
	payloads := make([][]byte, 0, len(chunks))
	for _, c := range chunks {
		raw, ok := c["Payload"].([]byte)
		if !ok {
			continue
		}
		payloads = append(payloads, raw)
	}
	return aggregateSSEPayloads(payloads)
}
