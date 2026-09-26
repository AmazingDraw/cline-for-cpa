package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
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

func TestErrorEnvelopeBuilders(t *testing.T) {
	raw := ErrorEnvelope("missing_credentials", "no key")
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if env.OK || env.Error == nil {
		t.Fatalf("envelope=%s", raw)
	}
	if env.Error.Code != "missing_credentials" || env.Error.Message != "no key" {
		t.Fatalf("code=%q message=%q", env.Error.Code, env.Error.Message)
	}
	if env.Error.HTTPStatus != 0 {
		t.Fatalf("plain ErrorEnvelope should omit http_status, got %d", env.Error.HTTPStatus)
	}
}

func TestStallEnvelopeAndClassifyStallEdges(t *testing.T) {
	raw := StallEnvelope(nil)
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if env.OK || env.Error == nil || env.Error.Code != "stream_stall" {
		t.Fatalf("nil stall envelope=%s", raw)
	}
	if env.Error.HTTPStatus != http.StatusGatewayTimeout {
		t.Fatalf("http_status=%d", env.Error.HTTPStatus)
	}

	unknown := ClassifyStall(&streamguard.StallError{Kind: streamguard.Kind("other")})
	if unknown.code != "other" || unknown.message != "上游流停滞。" {
		t.Fatalf("unknown stall: %+v", unknown)
	}
	withMsg := ClassifyStall(&streamguard.StallError{Kind: "x", Message: "custom stall"})
	if withMsg.message != "custom stall" {
		t.Fatalf("message=%q", withMsg.message)
	}

	silentNoDur := ClassifyStall(&streamguard.StallError{Kind: streamguard.KindSilence})
	if !strings.Contains(silentNoDur.message, "上游静默超时") || strings.Contains(silentNoDur.message, "约") {
		t.Fatalf("silence without duration: %q", silentNoDur.message)
	}
	hbNoDur := ClassifyStall(&streamguard.StallError{Kind: streamguard.KindHeartbeatOnly})
	if !strings.Contains(hbNoDur.message, "心跳空转") || strings.Contains(hbNoDur.message, "约") {
		t.Fatalf("heartbeat without duration: %q", hbNoDur.message)
	}
}

func TestOpenAIClassificationRemainingStatuses(t *testing.T) {
	cases := []struct {
		status   int
		wantType string
		wantCode string
	}{
		{http.StatusForbidden, "permission_error", "insufficient_quota"},
		{http.StatusNotFound, "invalid_request_error", "model_not_found"},
		{http.StatusInternalServerError, "server_error", "internal_server_error"},
		{http.StatusBadGateway, "server_error", "internal_server_error"},
	}
	for _, tc := range cases {
		gotType, gotCode := openAIClassification(tc.status)
		if gotType != tc.wantType || gotCode != tc.wantCode {
			t.Fatalf("status %d: type=%q code=%q want %q/%q", tc.status, gotType, gotCode, tc.wantType, tc.wantCode)
		}
	}
}

func TestFailureEnvelopeFallsBackToRequestError(t *testing.T) {
	raw := FailureEnvelope(failure{status: http.StatusBadRequest, message: "bad"})
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if env.Error == nil || env.Error.Code != "request_error" {
		t.Fatalf("want request_error, got %+v", env.Error)
	}
}

func TestClassifyUpstreamHTTPStatusMatrix(t *testing.T) {
	cases := []struct {
		status    int
		retryable bool
		substr    string
	}{
		{http.StatusUnauthorized, false, "401"},
		{http.StatusForbidden, false, "403"},
		{http.StatusBadGateway, false, "502"},
		{http.StatusGatewayTimeout, false, "504"},
		{http.StatusInternalServerError, false, "500"},
		{http.StatusBadRequest, false, "400"},
		{http.StatusOK, false, "200"},
	}
	for _, tc := range cases {
		f := ClassifyUpstreamHTTP(tc.status, "detail")
		if f.status != tc.status {
			t.Fatalf("%d: status=%d", tc.status, f.status)
		}
		if f.retryable == nil || *f.retryable != tc.retryable {
			t.Fatalf("%d: retryable=%v want %v", tc.status, f.retryable, tc.retryable)
		}
		if !strings.Contains(f.message, tc.substr) || !strings.Contains(f.message, "detail") {
			t.Fatalf("%d: message=%q", tc.status, f.message)
		}
	}
}

func TestClassifyClineTierLimitResetHint(t *testing.T) {
	free, ok := classifyClineTierLimit("")
	if ok || free.code != "" {
		t.Fatalf("empty body should not classify: %+v", free)
	}
	f, ok := classifyClineTierLimit(`FREE LIMIT REACHED ON MODEL foo, try again in 3 hours"`)
	if !ok || f.code != "cline_free_limit" || f.status != 429 {
		t.Fatalf("free limit: ok=%v %+v", ok, f)
	}
	if !strings.Contains(f.message, "免费模型额度已用尽") || !strings.Contains(f.message, "3 hours") {
		t.Fatalf("reset hint missing: %q", f.message)
	}
	pass, ok := classifyClineTierLimit("You have reached your ClinePass limit")
	if !ok || pass.code != "cline_pass_limit" {
		t.Fatalf("pass limit: ok=%v %+v", ok, pass)
	}
	if strings.Contains(pass.message, "预计") {
		t.Fatalf("no reset marker should omit hint: %q", pass.message)
	}
	if hint := clineResetHint("try again in "); hint != "" {
		t.Fatalf("empty tail should yield empty hint, got %q", hint)
	}
}

func TestUpstreamHTTPErrorString(t *testing.T) {
	var nilErr *upstreamHTTPError
	if nilErr.Error() != "upstream http error" {
		t.Fatalf("nil: %q", nilErr.Error())
	}
	if got := (&upstreamHTTPError{Status: 418}).Error(); got != "upstream status 418" {
		t.Fatalf("no body: %q", got)
	}
	if got := (&upstreamHTTPError{Status: 402, Body: "pay"}).Error(); got != "upstream status 402: pay" {
		t.Fatalf("with body: %q", got)
	}
}

func TestClassifyTransportBranches(t *testing.T) {
	nilF := ClassifyTransport(nil)
	if nilF.code != "upstream_stream_error" || nilF.status != http.StatusBadGateway {
		t.Fatalf("nil: %+v", nilF)
	}

	canceled := ClassifyTransport(context.Canceled)
	if canceled.status != 499 || canceled.code != "canceled" {
		t.Fatalf("canceled: %+v", canceled)
	}

	deadline := ClassifyTransport(context.DeadlineExceeded)
	if deadline.code != "upstream_timeout" || deadline.status != http.StatusGatewayTimeout {
		t.Fatalf("deadline: %+v", deadline)
	}

	timeout := ClassifyTransport(timeoutNetErr{})
	if timeout.code != "upstream_timeout" {
		t.Fatalf("net timeout: %+v", timeout)
	}

	legacy := ClassifyTransport(fmt.Errorf("upstream status 503: overloaded"))
	if legacy.status != 503 || !strings.Contains(legacy.message, "overloaded") {
		t.Fatalf("legacy parse: %+v", legacy)
	}
	bare := ClassifyTransport(fmt.Errorf("upstream status 502"))
	if bare.status != 502 {
		t.Fatalf("legacy bare: %+v", bare)
	}
	if _, _, ok := parseUpstreamStatusError("upstream status abc"); ok {
		t.Fatal("garbage status should not parse")
	}
	if _, _, ok := parseUpstreamStatusError("not a match"); ok {
		t.Fatal("prefix miss should not parse")
	}

	dial := ClassifyTransport(&net.OpError{Op: "dial", Net: "tcp", Err: errors.New("refused")})
	if dial.code != "upstream_dial_error" {
		t.Fatalf("op dial: %+v", dial)
	}
	dns := ClassifyTransport(&net.DNSError{Err: "no such host", Name: "missing.example"})
	if dns.code != "upstream_dial_error" {
		t.Fatalf("dns: %+v", dns)
	}

	generic := ClassifyTransport(errors.New("broken pipe"))
	if generic.code != "upstream_stream_error" || !strings.Contains(generic.message, "broken pipe") {
		t.Fatalf("generic: %+v", generic)
	}
}

type timeoutNetErr struct{}

func (timeoutNetErr) Error() string   { return "read i/o timeout" }
func (timeoutNetErr) Timeout() bool   { return true }
func (timeoutNetErr) Temporary() bool { return true }

func TestClassifyUpstreamHTTPForSourceAfterRefresh(t *testing.T) {
	oauthDead := ClassifyUpstreamHTTPForSource(401, "rejected", "oauth")
	if oauthDead.status != 401 || oauthDead.code != "cline_reauth_required" {
		t.Fatalf("recovered oauth 401: %+v", oauthDead)
	}
	transient := ClassifyUpstreamHTTPForSourceAfterRefresh(401, "lock held", "oauth_stale", false)
	if transient.status != http.StatusServiceUnavailable || transient.code != "oauth_refresh_unavailable" {
		t.Fatalf("unrecovered oauth 401: %+v", transient)
	}
	key := ClassifyUpstreamHTTPForSourceAfterRefresh(401, "bad key", "api_key", true)
	if key.code != "invalid_api_key" || key.status != 401 {
		t.Fatalf("api key 401: %+v", key)
	}
	passthrough := ClassifyUpstreamHTTPForSourceAfterRefresh(429, "slow", "oauth", true)
	if passthrough.status != 429 {
		t.Fatalf("non-401 should pass through: %+v", passthrough)
	}
}

func TestLocalFailureConstructors(t *testing.T) {
	reauth := reauthRequiredFailure()
	if reauth.code != "cline_reauth_required" || reauth.status != 401 {
		t.Fatalf("reauth: %+v", reauth)
	}

	model := invalidModelFailure("")
	if model.code != "invalid_model" || model.status != 400 || model.message != "模型无效或不在服务命名空间内。" {
		t.Fatalf("invalid model empty: %+v", model)
	}
	modelD := invalidModelFailure("no such id")
	if !strings.Contains(modelD.message, "no such id") {
		t.Fatalf("invalid model detail: %q", modelD.message)
	}

	req := invalidRequestFailure("")
	if req.code != "invalid_request" || req.message != "请求无效。" {
		t.Fatalf("invalid request empty: %+v", req)
	}
	reqD := invalidRequestFailure("missing model")
	if !strings.Contains(reqD.message, "missing model") {
		t.Fatalf("invalid request detail: %q", reqD.message)
	}
}
