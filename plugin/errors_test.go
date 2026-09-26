package plugin

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"cline-for-cpa/plugin/streamguard"
)

func TestClassifyStallKinds(t *testing.T) {
	cases := []struct {
		kind      streamguard.Kind
		retryable bool
		status    int
		code      string
	}{
		{streamguard.KindFirstFrame, true, 504, "first_frame_timeout"},
		{streamguard.KindSilence, true, 504, "stream_silence_timeout"},
		{streamguard.KindHeartbeatOnly, false, 504, "stream_heartbeat_only_timeout"},
	}
	for _, tc := range cases {
		f := ClassifyStall(&streamguard.StallError{
			Kind:          tc.kind,
			SilenceFor:    50 * time.Second,
			MeaningfulFor: 200 * time.Second,
			Message:       "raw",
		})
		if f.status != tc.status {
			t.Fatalf("%s: status=%d want %d", tc.kind, f.status, tc.status)
		}
		if f.code != tc.code {
			t.Fatalf("%s: code=%q want %q", tc.kind, f.code, tc.code)
		}
		if f.retryable == nil || *f.retryable != tc.retryable {
			t.Fatalf("%s: retryable=%v want %v", tc.kind, f.retryable, tc.retryable)
		}
		if f.message == "" || !containsHan(f.message) {
			t.Fatalf("%s: expected Chinese message, got %q", tc.kind, f.message)
		}
	}
}

func TestClassifyUpstreamHTTP402429(t *testing.T) {
	f402 := ClassifyUpstreamHTTP(http.StatusPaymentRequired, "need credits")
	if f402.status != 402 {
		t.Fatalf("402 status=%d", f402.status)
	}
	if f402.retryable == nil || *f402.retryable {
		t.Fatalf("402 should not be retryable")
	}

	f429 := ClassifyUpstreamHTTP(http.StatusTooManyRequests, "slow down")
	if f429.status != 429 {
		t.Fatalf("429 status=%d", f429.status)
	}
	if f429.retryable == nil || !*f429.retryable {
		t.Fatalf("429 should be retryable")
	}

	f503 := ClassifyUpstreamHTTP(http.StatusServiceUnavailable, "")
	if f503.retryable == nil || !*f503.retryable {
		t.Fatalf("503 should be retryable")
	}

	f401 := ClassifyUpstreamHTTP(http.StatusUnauthorized, "")
	if f401.retryable == nil || *f401.retryable {
		t.Fatalf("401 should not be retryable")
	}
	f403 := ClassifyUpstreamHTTP(http.StatusForbidden, "")
	if f403.retryable == nil || *f403.retryable {
		t.Fatalf("403 should not be retryable")
	}
}

func TestErrorBodyTextShape(t *testing.T) {
	f := ClassifyStall(&streamguard.StallError{Kind: streamguard.KindFirstFrame})
	raw := errorBodyText(f)
	if !json.Valid([]byte(raw)) {
		t.Fatalf("not valid JSON: %s", raw)
	}
	var body errorBody
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Message == "" {
		t.Fatal("missing error.message")
	}
	if body.Error.Type != "server_error" {
		t.Fatalf("type=%q", body.Error.Type)
	}
	if body.Error.Code != "first_frame_timeout" {
		t.Fatalf("code=%q want plugin-stable first_frame_timeout", body.Error.Code)
	}
	if body.Error.Retryable == nil || !*body.Error.Retryable {
		t.Fatalf("retryable=%v", body.Error.Retryable)
	}
}

func TestErrorBodyTextPrefersPluginCode(t *testing.T) {
	f := missingCredentialsFailure()
	raw := errorBodyText(f)
	var body errorBody
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Type != "authentication_error" {
		t.Fatalf("type=%q", body.Error.Type)
	}
	if body.Error.Code != "missing_credentials" {
		t.Fatalf("code=%q want missing_credentials (not invalid_api_key)", body.Error.Code)
	}
}

func TestFailureEnvelopeUsesOpenAIMessage(t *testing.T) {
	envBytes := FailureEnvelope(ClassifyUpstreamHTTP(429, "x"))
	var env envelope
	if err := json.Unmarshal(envBytes, &env); err != nil {
		t.Fatal(err)
	}
	if env.OK || env.Error == nil {
		t.Fatalf("envelope=%v", env)
	}
	if env.Error.HTTPStatus != 429 {
		t.Fatalf("http_status=%d", env.Error.HTTPStatus)
	}
	if !json.Valid([]byte(env.Error.Message)) {
		t.Fatalf("message should be OpenAI JSON, got %q", env.Error.Message)
	}
}

func TestClassifyTransportUpstreamHTTP(t *testing.T) {
	err := &upstreamHTTPError{Status: 402, Body: "pay"}
	f := ClassifyTransport(err)
	if f.status != 402 {
		t.Fatalf("status=%d", f.status)
	}
	if f.retryable == nil || *f.retryable {
		t.Fatal("402 not retryable")
	}
}

func TestClassifyTransportDial(t *testing.T) {
	f := ClassifyTransport(errors.New("dial tcp 1.2.3.4:443: connect: connection refused"))
	if f.status != 502 || f.code != "upstream_dial_error" {
		t.Fatalf("got status=%d code=%q", f.status, f.code)
	}
	if f.retryable == nil || !*f.retryable {
		t.Fatal("dial should be retryable")
	}
}

func TestOpenAIClassification(t *testing.T) {
	typ, code := openAIClassification(401)
	if typ != "authentication_error" || code != "invalid_api_key" {
		t.Fatalf("%s %s", typ, code)
	}
	typ, code = openAIClassification(429)
	if typ != "rate_limit_error" || code != "rate_limit_exceeded" {
		t.Fatalf("%s %s", typ, code)
	}
	typ, code = openAIClassification(400)
	if typ != "invalid_request_error" || code != "" {
		t.Fatalf("%s %q", typ, code)
	}
}

func containsHan(s string) bool {
	for _, r := range s {
		if r >= 0x4e00 && r <= 0x9fff {
			return true
		}
	}
	return strings.Contains(s, "上游") // fallback
}
