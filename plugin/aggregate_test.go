package plugin

import (
	"encoding/json"
	"testing"
)

func chunk(payload string) []byte { return []byte(payload) }

func TestAggregateContentAndUsage(t *testing.T) {
	payloads := [][]byte{
		chunk(`{"id":"gen_1","object":"chat.completion.chunk","created":123,"model":"deepseek/x","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`),
		chunk(`{"id":"gen_1","choices":[{"index":0,"delta":{"content":"你好"},"finish_reason":null}]}`),
		chunk(`{"id":"gen_1","choices":[{"index":0,"delta":{"content":"，世界"},"finish_reason":null}]}`),
		chunk(`{"id":"gen_1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`),
		chunk(`[DONE]`),
	}
	out, err := aggregateSSEPayloads(payloads)
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	var doc struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if doc.Object != "chat.completion" {
		t.Fatalf("object = %q", doc.Object)
	}
	if doc.Choices[0].Message.Content != "你好，世界" {
		t.Fatalf("content = %q", doc.Choices[0].Message.Content)
	}
	if doc.Choices[0].Message.Role != "assistant" {
		t.Fatalf("role = %q", doc.Choices[0].Message.Role)
	}
	if doc.Choices[0].FinishReason != "stop" {
		t.Fatalf("finish_reason = %q", doc.Choices[0].FinishReason)
	}
	if doc.Usage == nil || doc.Usage["total_tokens"] != float64(7) {
		t.Fatalf("usage not preserved: %v", doc.Usage)
	}
	if doc.ID != "gen_1" || doc.Model != "deepseek/x" || doc.Created != 123 {
		t.Fatalf("metadata lost: %+v", doc)
	}
}

func TestAggregateReasoningBothKeys(t *testing.T) {
	payloads := [][]byte{
		chunk(`{"choices":[{"index":0,"delta":{"reasoning":"我们需要"},"finish_reason":null}]}`),
		chunk(`{"choices":[{"index":0,"delta":{"reasoning":"计算"},"finish_reason":null}]}`),
		chunk(`{"choices":[{"index":0,"delta":{"content":"答案是 2"},"finish_reason":"stop"}]}`),
	}
	out, err := aggregateSSEPayloads(payloads)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Choices []struct {
			Message map[string]any `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	msg := doc.Choices[0].Message
	if msg["reasoning_content"] != "我们需要计算" || msg["reasoning"] != "我们需要计算" {
		t.Fatalf("reasoning not aggregated: %+v", msg)
	}
	if msg["content"] != "答案是 2" {
		t.Fatalf("content = %v", msg["content"])
	}
}

func TestAggregateToolCallsAcrossFragments(t *testing.T) {
	payloads := [][]byte{
		chunk(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read","arguments":"{\"pa"}}]},"finish_reason":null}]}`),
		chunk(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"th\":\"a.txt\"}"}}]},"finish_reason":null}]}`),
		chunk(`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`),
	}
	out, err := aggregateSSEPayloads(payloads)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	calls := doc.Choices[0].Message.ToolCalls
	if len(calls) != 1 {
		t.Fatalf("tool_calls = %d, want 1", len(calls))
	}
	if calls[0].ID != "call_1" || calls[0].Function.Name != "read" {
		t.Fatalf("tool call identity lost: %+v", calls[0])
	}
	if calls[0].Function.Arguments != `{"path":"a.txt"}` {
		t.Fatalf("arguments = %q", calls[0].Function.Arguments)
	}
	if doc.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish_reason = %q", doc.Choices[0].FinishReason)
	}
}

func TestAggregateEmptyStreamErrors(t *testing.T) {
	if _, err := aggregateSSEPayloads([][]byte{chunk(`[DONE]`)}); err == nil {
		t.Fatal("empty stream should error so the caller reports a clear failure")
	}
}

func TestAggregateIgnoresNoise(t *testing.T) {
	payloads := [][]byte{
		chunk(``),
		chunk(`: keepalive`),
		chunk(`not-json`),
		chunk(`{"choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`),
	}
	out, err := aggregateSSEPayloads(payloads)
	if err != nil {
		t.Fatalf("noise must not abort aggregation: %v", err)
	}
	var doc struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Choices[0].Message.Content != "ok" {
		t.Fatalf("content = %q", doc.Choices[0].Message.Content)
	}
}

func TestForceStreamUpstreamDefaultsOn(t *testing.T) {
	if !forceStreamUpstream(defaultConfig()) {
		t.Fatal("force_stream_upstream must default to true (Cline rejects non-stream)")
	}
	off := false
	if forceStreamUpstream(pluginConfig{ForceStreamUpstream: &off}) {
		t.Fatal("explicit false must disable internal streaming")
	}
}

func TestAggregateMidStreamErrorSurfaced(t *testing.T) {
	payloads := [][]byte{
		chunk(`{"id":"gen_err","choices":[{"index":0,"delta":{"content":"partial output before failure"},"finish_reason":null}]}`),
		chunk(`{"id":"gen_err","choices":[{"index":0,"finish_reason":"error","error":{"code":"context_length_exceeded","message":"The input exceeds the model's maximum context length."}}]}`),
		chunk(`[DONE]`),
	}
	_, err := aggregateSSEPayloads(payloads)
	if err == nil {
		t.Fatal("expected mid-stream error to fail aggregation, but got nil error")
	}
	want := "upstream mid-stream error (context_length_exceeded): The input exceeds the model's maximum context length."
	if err.Error() != want {
		t.Fatalf("unexpected error message: got %q, want %q", err.Error(), want)
	}
}

func TestAggregateReasoningDetails(t *testing.T) {
	payloads := [][]byte{
		chunk(`{"choices":[{"index":0,"delta":{"reasoning":"思考过程","reasoning_details":{"encrypted":"xyz123"}},"finish_reason":null}]}`),
		chunk(`{"choices":[{"index":0,"delta":{"content":"答案"},"finish_reason":"stop"}]}`),
	}
	out, err := aggregateSSEPayloads(payloads)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Choices []struct {
			Message map[string]any `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	msg := doc.Choices[0].Message
	if msg["reasoning"] != "思考过程" {
		t.Fatalf("reasoning = %v", msg["reasoning"])
	}
	details, ok := msg["reasoning_details"].(map[string]any)
	if !ok || details["encrypted"] != "xyz123" {
		t.Fatalf("reasoning_details not preserved: %v", msg["reasoning_details"])
	}
}

func TestAggregateUsageNormalization(t *testing.T) {
	payloads := [][]byte{
		chunk(`{"choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30,"cost":0.00015}}`),
		chunk(`[DONE]`),
	}
	out, err := aggregateSSEPayloads(payloads)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Usage struct {
			PromptTokens        int     `json:"prompt_tokens"`
			CompletionTokens    int     `json:"completion_tokens"`
			TotalTokens         int     `json:"total_tokens"`
			Cost                float64 `json:"cost"`
			PromptTokensDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Usage.Cost != 0.00015 {
		t.Fatalf("cost = %v, want 0.00015", doc.Usage.Cost)
	}
	if doc.Usage.PromptTokensDetails.CachedTokens != 0 {
		t.Fatalf("cached_tokens = %v, want 0", doc.Usage.PromptTokensDetails.CachedTokens)
	}
}
