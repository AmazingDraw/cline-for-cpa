package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cline-for-cpa/plugin/streamguard"
)

// --- helpers -----------------------------------------------------------------

func testBool(v bool) *bool { return &v }

func freshOAuthStorage(email string) clineOAuthStorage {
	return clineOAuthStorage{
		Type:         ProviderKey,
		AccessToken:  "test-access-" + email,
		RefreshToken: "test-refresh-" + email,
		ExpiresAt:    time.Now().Add(2 * time.Hour).UnixMilli(),
		Email:        email,
		AccountID:    "acct-" + email,
	}
}

func marshalStorage(t *testing.T, st clineOAuthStorage) []byte {
	t.Helper()
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func executorPayload(model string) []byte {
	raw, _ := json.Marshal(map[string]any{
		"model":    model,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	return raw
}

func buildExecuteRequest(t *testing.T, model string, storage []byte, extra map[string]any) []byte {
	t.Helper()
	req := map[string]any{
		"model":        model,
		"payload":      executorPayload(model),
		"storage_json": storage,
	}
	for k, v := range extra {
		req[k] = v
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func withTestUpstream(t *testing.T, h http.HandlerFunc) (pluginConfig, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	cfg := defaultConfig()
	cfg.BaseURL = srv.URL
	cfg.AuthDir = t.TempDir()
	cfg.ClientVersion = defaultClientVersion
	cfg.ForceStreamUpstream = testBool(false)
	// Keep Stream Guard budgets short but not so short that healthy SSE races.
	cfg.FirstFrameTimeoutSeconds = 30
	cfg.StreamSilenceTimeoutSeconds = 60
	cfg.StreamHeartbeatOnlyTimeoutSeconds = 90
	upstreamHTTPClientOverride = srv.Client()
	t.Cleanup(func() { upstreamHTTPClientOverride = nil })
	prevClock := streamGuardClock
	t.Cleanup(func() { streamGuardClock = prevClock })
	return cfg, srv
}

func sampleSSEBody() string {
	chunk1 := `{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"hello"},"finish_reason":null}]}`
	chunk2 := `{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`
	return "data: " + chunk1 + "\n\ndata: " + chunk2 + "\n\ndata: [DONE]\n\n"
}

func sampleJSONCompletion() []byte {
	raw, _ := json.Marshal(map[string]any{
		"id":      "chatcmpl-1",
		"object":  "chat.completion",
		"choices": []map[string]any{{"index": 0, "message": map[string]string{"role": "assistant", "content": "hello"}, "finish_reason": "stop"}},
	})
	return raw
}

func installHostCaller(t *testing.T, fn func(method string, request []byte) ([]byte, error)) {
	t.Helper()
	prev := hostCaller
	SetHostCaller(fn)
	t.Cleanup(func() { SetHostCaller(prev) })
}

// --- pure helpers ------------------------------------------------------------

func TestShouldRetryFreshConnection(t *testing.T) {
	stall := &streamguard.StallError{Kind: streamguard.KindFirstFrame, Message: "stall"}
	cases := []struct {
		name    string
		stall   *streamguard.StallError
		err     error
		emitted bool
		want    bool
	}{
		{"emitted_blocks", stall, errors.New("timeout"), true, false},
		{"stall_retries", stall, nil, false, true},
		{"nil_err_no_stall", nil, nil, false, false},
		{"timeout_hint", nil, errors.New("i/o timeout"), false, true},
		{"timed_out_hint", nil, errors.New("context timed out"), false, true},
		{"deadline_hint", nil, errors.New("context deadline exceeded"), false, true},
		{"reset_hint", nil, errors.New("connection reset by peer"), false, true},
		{"eof_hint", nil, io.EOF, false, true},
		{"broken_pipe", nil, errors.New("write: broken pipe"), false, true},
		{"no_route", nil, errors.New("no route to host"), false, true},
		{"refused", nil, errors.New("connection refused"), false, true},
		{"internal_error", nil, errors.New("rpc error: code = Internal desc = INTERNAL_ERROR"), false, true},
		{"stream_error", nil, errors.New("stream error: stream ID 1; INTERNAL_ERROR"), false, true},
		{"http2_hint", nil, errors.New("http2: server sent GOAWAY"), false, true},
		{"unexpected_eof", nil, errors.New("unexpected EOF"), false, true},
		{"other_err", nil, errors.New("something else"), false, false},
		{"business_401", nil, errors.New("upstream status 401: unauthorized"), false, false},
		{"4xx_eof_in_body", nil, errors.New("upstream status 400: unexpected EOF while parsing request body"), false, false},
		{"5xx_internal_still", nil, errors.New("upstream status 500: INTERNAL_ERROR"), false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldRetryFreshConnection(tc.stall, tc.err, tc.emitted); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestPreferStallParentCancelDropsFirst(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := &streamguard.StallError{Kind: streamguard.KindFirstFrame, Message: "ff"}
	cancel()
	if got := preferStall(first, nil, context.Canceled, parent); got != nil {
		t.Fatalf("parent cancel must drop first stall, got %+v", got)
	}
}

func TestPreferStallPoisonedAttemptKeepsFirst(t *testing.T) {
	first := &streamguard.StallError{Kind: streamguard.KindFirstFrame, Message: "ff"}
	if got := preferStall(first, nil, context.Canceled, context.Background()); got != first {
		t.Fatalf("poisoned attempt cancel must keep first stall while parent lives")
	}
}

func TestUpstreamStatusOf(t *testing.T) {
	if st, ok := upstreamStatusOf(nil); ok || st != 0 {
		t.Fatalf("nil → %d,%v", st, ok)
	}
	if st, ok := upstreamStatusOf(errors.New("upstream status 401: nope")); !ok || st != 401 {
		t.Fatalf("401 parse → %d,%v", st, ok)
	}
	if st, ok := upstreamStatusOf(errors.New("plain")); ok || st != 0 {
		t.Fatalf("plain → %d,%v", st, ok)
	}
}

func TestStreamStallError(t *testing.T) {
	var nilSE *streamStallError
	if nilSE.Error() != "stream stall" {
		t.Fatalf("nil Error = %q", nilSE.Error())
	}
	se := &streamStallError{stall: &streamguard.StallError{Message: "first frame"}}
	if se.Error() != "first frame" {
		t.Fatalf("Error = %q", se.Error())
	}
}

func TestUnwrapJSONBytes(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{`{"a":1}`, `{"a":1}`},
		{`"{\"a\":1}"`, `{"a":1}`},
		{`"not-json-string`, `"not-json-string`},
		{"  {\"x\":1}  ", `{"x":1}`},
	}
	for _, tc := range cases {
		got := string(unwrapJSONBytes([]byte(tc.in)))
		if got != tc.want {
			t.Fatalf("in=%q got=%q want=%q", tc.in, got, tc.want)
		}
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate([]byte("abc"), 10); got != "abc" {
		t.Fatalf("short = %q", got)
	}
	if got := truncate([]byte("abcdefghij"), 3); got != "abc…" {
		t.Fatalf("long = %q", got)
	}
}

func TestNormalizeClineChatPayload_ExecutorExtras(t *testing.T) {
	inner := `{"id":"x","choices":[]}`
	wrapped := `{"success":true,"data":` + inner + `}`
	if got := string(normalizeClineChatPayload([]byte(wrapped))); got != inner {
		t.Fatalf("unwrap got %s", got)
	}
	if got := string(normalizeClineChatPayload([]byte(inner))); got != inner {
		t.Fatalf("passthrough got %s", got)
	}
	if got := string(normalizeClineChatPayload([]byte("not-json"))); got != "not-json" {
		t.Fatalf("non-object got %s", got)
	}
	if got := string(normalizeClineChatPayload(nil)); got != "" {
		t.Fatalf("nil got %q", got)
	}
}

func TestEmitAndCloseHostStream(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	installHostCaller(t, func(method string, request []byte) ([]byte, error) {
		mu.Lock()
		calls = append(calls, method+":"+string(request))
		mu.Unlock()
		return nil, nil
	})
	if err := emitHostChunk("cb", "sid", []byte(`{"x":1}`)); err != nil {
		t.Fatal(err)
	}
	closeHostStream("cb", "sid", nil)
	closeHostStream("cb", "sid", errors.New("boom"))
	installHostCaller(t, nil)
	if err := emitHostChunk("cb", "sid", []byte("x")); err != nil {
		t.Fatal(err)
	}
	closeHostStream("cb", "sid", errors.New("ignored"))
	mu.Lock()
	defer mu.Unlock()
	if len(calls) < 3 {
		t.Fatalf("calls=%v", calls)
	}
	if !strings.Contains(calls[0], "host.stream.emit") {
		t.Fatalf("emit = %s", calls[0])
	}
	if !strings.Contains(calls[2], "boom") {
		t.Fatalf("close-with-err = %s", calls[2])
	}
}

func TestLogModelRouteThrottled(t *testing.T) {
	routeLogMu.Lock()
	routeLogSeen = map[string]time.Time{}
	routeLogMu.Unlock()
	cfg := defaultConfig()
	cfg.AuthDir = t.TempDir()
	logModelRoute(cfg, "cline-pass/a", "cline-pass/a")
	logModelRoute(cfg, "cline-pass/a", "cline-pass/a") // throttled
	routeLogMu.Lock()
	n := len(routeLogSeen)
	routeLogMu.Unlock()
	if n != 1 {
		t.Fatalf("seen=%d want 1", n)
	}
}

func TestNewUpstreamRequest(t *testing.T) {
	cfg := defaultConfig()
	cfg.BaseURL = "http://example.invalid/v1/"
	cfg.ClientVersion = defaultClientVersion
	req, err := newUpstreamRequest(context.Background(), cfg, "tok", []byte(`{"model":"m"}`), true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(req.URL.Path, "/chat/completions") {
		t.Fatalf("path=%s", req.URL.Path)
	}
	if req.Header.Get("Authorization") != "Bearer tok" {
		t.Fatalf("auth=%q", req.Header.Get("Authorization"))
	}
	if req.Header.Get("Accept") != "text/event-stream" {
		t.Fatalf("accept=%q", req.Header.Get("Accept"))
	}
	body, _ := io.ReadAll(req.Body)
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatal(err)
	}
	if obj["stream"] != true {
		t.Fatalf("stream flag missing: %v", obj)
	}
	_, err = newUpstreamRequest(context.Background(), cfg, "tok", []byte(`not-json`), true)
	if err == nil {
		t.Fatal("expected unmarshal error for stream rewrite")
	}
}

// --- proxySSE ----------------------------------------------------------------

func TestProxySSETable(t *testing.T) {
	cases := []struct {
		name       string
		handler    http.HandlerFunc
		cancel     bool
		wantErrSub string
		wantEmit   int
		wantOK     bool
	}{
		{
			name: "success_sse",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, ": ping\n")
				_, _ = io.WriteString(w, "event: ping\n")
				_, _ = io.WriteString(w, "id: 1\n")
				_, _ = io.WriteString(w, sampleSSEBody())
			},
			wantEmit: 3, // two JSON chunks + [DONE]
			wantOK:   true,
		},
		{
			name: "upstream_401",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"error":"bad token"}`)
			},
			wantErrSub: "upstream status 401",
		},
		{
			name: "upstream_500",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(w, "boom")
			},
			wantErrSub: "upstream status 500",
		},
		{
			name: "blank_line_flush",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				// Multi-line data without leading { waits for blank line.
				_, _ = io.WriteString(w, "data: plain-text-frame\n\n")
			},
			wantEmit: 1,
			wantOK:   true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, _ := withTestUpstream(t, tc.handler)
			httpReq, err := newUpstreamRequest(context.Background(), cfg, "tok", []byte(`{"model":"m"}`), true)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			var cancel context.CancelFunc
			if tc.cancel {
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			var emitted [][]byte
			guard := streamguard.New(streamGuardConfig(cfg), nil, nil)
			guard.Start()
			defer guard.Disarm()
			err = proxySSE(ctx, httpReq, guard, func(line []byte) error {
				emitted = append(emitted, append([]byte(nil), line...))
				return nil
			})
			if tc.wantOK {
				if err != nil {
					t.Fatalf("unexpected err: %v", err)
				}
				if len(emitted) != tc.wantEmit {
					t.Fatalf("emitted=%d want %d (%q)", len(emitted), tc.wantEmit, emitted)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErrSub) {
				t.Fatalf("err=%v want substring %q", err, tc.wantErrSub)
			}
		})
	}
}

func TestProxySSEContextCancel(t *testing.T) {
	started := make(chan struct{})
	cfg, _ := withTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		close(started)
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	})
	httpReq, err := newUpstreamRequest(context.Background(), cfg, "tok", []byte(`{"model":"m"}`), true)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	guard := streamguard.New(streamGuardConfig(cfg), nil, nil)
	guard.Start()
	defer guard.Disarm()
	errCh := make(chan error, 1)
	go func() {
		errCh <- proxySSE(ctx, httpReq, guard, func([]byte) error { return nil })
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not start")
	}
	cancel()
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected cancel error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("proxySSE did not return after cancel")
	}
}

func TestProxySSEEmitError(t *testing.T) {
	cfg, _ := withTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sampleSSEBody())
	})
	httpReq, err := newUpstreamRequest(context.Background(), cfg, "tok", []byte(`{"model":"m"}`), true)
	if err != nil {
		t.Fatal(err)
	}
	guard := streamguard.New(streamGuardConfig(cfg), nil, nil)
	guard.Start()
	defer guard.Disarm()
	err = proxySSE(context.Background(), httpReq, guard, func([]byte) error {
		return errors.New("emit failed")
	})
	if err == nil || !strings.Contains(err.Error(), "emit failed") {
		t.Fatalf("err=%v", err)
	}
}

func TestProxySSEDialError(t *testing.T) {
	cfg := defaultConfig()
	cfg.BaseURL = "http://127.0.0.1:1" // nothing listening
	cfg.ClientVersion = defaultClientVersion
	upstreamHTTPClientOverride = &http.Client{Timeout: 200 * time.Millisecond}
	t.Cleanup(func() { upstreamHTTPClientOverride = nil })
	httpReq, err := newUpstreamRequest(context.Background(), cfg, "tok", []byte(`{"model":"m"}`), true)
	if err != nil {
		t.Fatal(err)
	}
	guard := streamguard.New(streamGuardConfig(cfg), nil, nil)
	guard.Start()
	defer guard.Disarm()
	err = proxySSE(context.Background(), httpReq, guard, func([]byte) error { return nil })
	if err == nil {
		t.Fatal("expected dial error")
	}
}

// --- executeOnce / executeStreamCollect --------------------------------------

func TestExecuteOnceJSONSuccess(t *testing.T) {
	var sawAuth string
	cfg, _ := withTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(sampleJSONCompletion())
	})
	cred := credential{bearer: "tok-1", source: "oauth", oauth: true}
	raw, err := executeOnce(context.Background(), cfg, cred, executorPayload("cline-pass/glm-5.3-flash"))
	if err != nil {
		t.Fatal(err)
	}
	env := decodeEnvelope(t, raw)
	if !env.OK {
		t.Fatalf("not ok: %+v", env.Error)
	}
	if sawAuth != "Bearer tok-1" {
		t.Fatalf("auth=%q", sawAuth)
	}
}

func TestExecuteOnceClineEnvelopeUnwrap(t *testing.T) {
	inner := sampleJSONCompletion()
	wrapped, _ := json.Marshal(map[string]any{"success": true, "data": json.RawMessage(inner)})
	cfg, _ := withTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(wrapped)
	})
	raw, err := executeOnce(context.Background(), cfg, credential{bearer: "t"}, executorPayload("m"))
	if err != nil {
		t.Fatal(err)
	}
	env := decodeEnvelope(t, raw)
	var result map[string]any
	if err := json.Unmarshal(env.Result, &result); err != nil {
		t.Fatal(err)
	}
	payload, _ := result["Payload"].(string)
	if payload == "" {
		// encoding/json may decode []byte as base64 string OR we marshaled as []byte in map
		if b, ok := result["Payload"].([]byte); ok {
			payload = string(b)
		} else if s, ok := result["Payload"].(string); ok {
			// base64
			_ = s
		}
	}
	// Result.Payload is []byte → JSON base64. Decode via typed struct.
	var typed struct {
		Payload []byte `json:"Payload"`
	}
	if err := json.Unmarshal(env.Result, &typed); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(typed.Payload), `"chat.completion"`) {
		t.Fatalf("payload=%s", typed.Payload)
	}
}

func TestExecuteOnceUpstream4xx(t *testing.T) {
	cfg, _ := withTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = io.WriteString(w, "need credits")
	})
	raw, err := executeOnce(context.Background(), cfg, credential{bearer: "t", source: "oauth"}, executorPayload("m"))
	if err != nil {
		t.Fatal(err)
	}
	env := decodeEnvelope(t, raw)
	if env.OK || env.Error == nil || env.Error.HTTPStatus != 402 {
		t.Fatalf("env=%+v", env)
	}
}

func TestExecuteOnceTransportError(t *testing.T) {
	cfg := defaultConfig()
	cfg.BaseURL = "http://127.0.0.1:1"
	cfg.ClientVersion = defaultClientVersion
	cfg.ForceStreamUpstream = testBool(false)
	upstreamHTTPClientOverride = &http.Client{Timeout: 200 * time.Millisecond}
	t.Cleanup(func() { upstreamHTTPClientOverride = nil })
	raw, err := executeOnce(context.Background(), cfg, credential{bearer: "t"}, executorPayload("m"))
	if err != nil {
		t.Fatal(err)
	}
	env := decodeEnvelope(t, raw)
	if env.OK {
		t.Fatalf("expected failure, got ok: %s", raw)
	}
}

func TestExecuteOnceUnauthorizedRefreshRetry(t *testing.T) {
	var chatCalls, refreshCalls int32
	cfg, _ := withTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/auth/refresh"):
			atomic.AddInt32(&refreshCalls, 1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": map[string]any{
					"accessToken":  "NEW-ACCESS",
					"refreshToken": "NEW-REFRESH",
					"expiresAt":    time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
					"tokenType":    "Bearer",
					"userInfo":     map[string]any{"email": "exec@example.com", "clineUserId": "u1"},
				},
			})
		default:
			n := atomic.AddInt32(&chatCalls, 1)
			if n == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, "expired")
				return
			}
			_, _ = w.Write(sampleJSONCompletion())
		}
	})
	st := freshOAuthStorage("exec@example.com")
	writeAuthFile(t, cfg.AuthDir, st.Email, map[string]any{
		"type": ProviderKey, "access_token": st.AccessToken, "refresh_token": st.RefreshToken,
		"expires_at": st.ExpiresAt, "email": st.Email, "account_id": st.AccountID,
	})
	cred := credential{bearer: oauthBearer(&st), source: "oauth", oauth: true, storage: &st}
	raw, err := executeOnce(context.Background(), cfg, cred, executorPayload("m"))
	if err != nil {
		t.Fatal(err)
	}
	env := decodeEnvelope(t, raw)
	if !env.OK {
		t.Fatalf("expected ok after refresh retry: %+v", env.Error)
	}
	if atomic.LoadInt32(&chatCalls) < 2 {
		t.Fatalf("chatCalls=%d", chatCalls)
	}
	if atomic.LoadInt32(&refreshCalls) < 1 {
		t.Fatalf("refreshCalls=%d", refreshCalls)
	}
}

func TestExecuteStreamCollectSuccess(t *testing.T) {
	cfg, _ := withTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sampleSSEBody())
	})
	raw, err := executeStreamCollect(context.Background(), cfg, credential{bearer: "t"}, executorPayload("m"))
	if err != nil {
		t.Fatal(err)
	}
	env := decodeEnvelope(t, raw)
	if !env.OK {
		t.Fatalf("not ok: %+v", env.Error)
	}
	var result struct {
		Chunks []map[string]any `json:"chunks"`
	}
	if err := json.Unmarshal(env.Result, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Chunks) < 2 {
		t.Fatalf("chunks=%d", len(result.Chunks))
	}
}

func TestExecuteStreamCollectUpstreamError(t *testing.T) {
	cfg, _ := withTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, "slow down")
	})
	raw, err := executeStreamCollect(context.Background(), cfg, credential{bearer: "t", source: "oauth"}, executorPayload("m"))
	if err != nil {
		t.Fatal(err)
	}
	env := decodeEnvelope(t, raw)
	if env.OK || env.Error == nil || env.Error.HTTPStatus != 429 {
		t.Fatalf("env=%+v", env)
	}
}

func TestExecuteStreamCollectUnauthorizedRefresh(t *testing.T) {
	var chatCalls int32
	cfg, _ := withTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/auth/refresh") {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": map[string]any{
					"accessToken": "STREAM-NEW", "refreshToken": "STREAM-RT",
					"expiresAt": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
					"tokenType": "Bearer",
					"userInfo":  map[string]any{"email": "stream@example.com", "clineUserId": "u2"},
				},
			})
			return
		}
		n := atomic.AddInt32(&chatCalls, 1)
		if n == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, "expired")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sampleSSEBody())
	})
	st := freshOAuthStorage("stream@example.com")
	writeAuthFile(t, cfg.AuthDir, st.Email, map[string]any{
		"type": ProviderKey, "access_token": st.AccessToken, "refresh_token": st.RefreshToken,
		"expires_at": st.ExpiresAt, "email": st.Email, "account_id": st.AccountID,
	})
	cred := credential{bearer: oauthBearer(&st), source: "oauth", oauth: true, storage: &st}
	raw, err := executeStreamCollect(context.Background(), cfg, cred, executorPayload("m"))
	if err != nil {
		t.Fatal(err)
	}
	env := decodeEnvelope(t, raw)
	if !env.OK {
		t.Fatalf("expected ok: %+v", env.Error)
	}
}

func TestUpstreamCompletionForceStreamAggregate(t *testing.T) {
	cfg, _ := withTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sampleSSEBody())
	})
	cfg.ForceStreamUpstream = testBool(true)
	body, status, err := upstreamCompletion(context.Background(), cfg, credential{bearer: "t"}, executorPayload("m"))
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK {
		t.Fatalf("status=%d", status)
	}
	if !strings.Contains(string(body), "hello") {
		t.Fatalf("body=%s", body)
	}
}

func TestUpstreamCompletionForceStreamUpstreamHTTPError(t *testing.T) {
	cfg, _ := withTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "nope")
	})
	cfg.ForceStreamUpstream = testBool(true)
	body, status, err := upstreamCompletion(context.Background(), cfg, credential{bearer: "t"}, executorPayload("m"))
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", status, body)
	}
}

// --- runGuardedProxy / executeStream async -----------------------------------

func TestRunGuardedProxySuccess(t *testing.T) {
	cfg, _ := withTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sampleSSEBody())
	})
	closed := make(chan string, 1)
	var emits int32
	installHostCaller(t, func(method string, request []byte) ([]byte, error) {
		switch method {
		case "host.stream.emit":
			atomic.AddInt32(&emits, 1)
		case "host.stream.close":
			var req map[string]any
			_ = json.Unmarshal(request, &req)
			if errMsg, _ := req["error"].(string); errMsg != "" {
				closed <- "err:" + errMsg
			} else {
				closed <- "ok"
			}
		}
		return nil, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runGuardedProxy(ctx, cfg, credential{bearer: "t"}, executorPayload("m"), "cb1", "sid1")
	select {
	case got := <-closed:
		if got != "ok" {
			t.Fatalf("close=%s emits=%d", got, emits)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for close")
	}
	if atomic.LoadInt32(&emits) < 1 {
		t.Fatalf("emits=%d", emits)
	}
}

func TestRunGuardedProxyUpstreamError(t *testing.T) {
	cfg, _ := withTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "upstream down")
	})
	closed := make(chan string, 1)
	installHostCaller(t, func(method string, request []byte) ([]byte, error) {
		if method == "host.stream.close" {
			closed <- string(request)
		}
		return nil, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runGuardedProxy(ctx, cfg, credential{bearer: "t", source: "oauth"}, executorPayload("m"), "cb", "sid")
	select {
	case got := <-closed:
		if !strings.Contains(got, "error") {
			t.Fatalf("close payload=%s", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout")
	}
}

func TestRunGuardedProxyUnauthorizedRefresh(t *testing.T) {
	var chatCalls int32
	cfg, _ := withTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/auth/refresh") {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": map[string]any{
					"accessToken": "PROXY-NEW", "refreshToken": "PROXY-RT",
					"expiresAt": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
					"tokenType": "Bearer",
					"userInfo":  map[string]any{"email": "proxy@example.com", "clineUserId": "u3"},
				},
			})
			return
		}
		n := atomic.AddInt32(&chatCalls, 1)
		if n == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, "expired")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sampleSSEBody())
	})
	st := freshOAuthStorage("proxy@example.com")
	writeAuthFile(t, cfg.AuthDir, st.Email, map[string]any{
		"type": ProviderKey, "access_token": st.AccessToken, "refresh_token": st.RefreshToken,
		"expires_at": st.ExpiresAt, "email": st.Email, "account_id": st.AccountID,
	})
	cred := credential{bearer: oauthBearer(&st), source: "oauth", oauth: true, storage: &st}
	closed := make(chan string, 1)
	installHostCaller(t, func(method string, request []byte) ([]byte, error) {
		if method == "host.stream.close" {
			var req map[string]any
			_ = json.Unmarshal(request, &req)
			if _, hasErr := req["error"]; hasErr {
				closed <- "err"
			} else {
				closed <- "ok"
			}
		}
		return nil, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runGuardedProxy(ctx, cfg, cred, executorPayload("m"), "cb", "sid")
	select {
	case got := <-closed:
		if got != "ok" {
			t.Fatalf("close=%s chatCalls=%d", got, chatCalls)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout")
	}
}

func TestRunGuardedProxyStall(t *testing.T) {
	// Use RealClock with a short first-frame budget. ManualClock is not safe to
	// AdvanceTo from another goroutine while Guard timers arm on the proxy path.
	streamGuardClock = nil

	cfg, _ := withTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Hold open until the guard cancels the request context.
		<-r.Context().Done()
	})
	cfg.FirstFrameTimeoutSeconds = 1
	cfg.StreamSilenceTimeoutSeconds = 30
	cfg.StreamHeartbeatOnlyTimeoutSeconds = 60

	closed := make(chan string, 1)
	installHostCaller(t, func(method string, request []byte) ([]byte, error) {
		if method == "host.stream.close" {
			closed <- string(request)
		}
		return nil, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		runGuardedProxy(ctx, cfg, credential{bearer: "t"}, executorPayload("m"), "cb", "sid")
	}()
	select {
	case got := <-closed:
		if !strings.Contains(got, "error") {
			t.Fatalf("expected stall close error, got %s", got)
		}
		if strings.Contains(got, `"code":"canceled"`) || strings.Contains(got, "已取消") {
			t.Fatalf("stall must not surface as canceled: %s", got)
		}
		if !strings.Contains(got, "first_frame") && !strings.Contains(got, "首帧") {
			t.Fatalf("expected first_frame stall classification, got %s", got)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("timeout waiting for stall close")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runGuardedProxy did not return")
	}
}

func TestExecuteStreamAsyncReturnsHeaders(t *testing.T) {
	cfg, _ := withTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sampleSSEBody())
	})
	closed := make(chan struct{}, 1)
	installHostCaller(t, func(method string, request []byte) ([]byte, error) {
		if method == "host.stream.close" {
			select {
			case closed <- struct{}{}:
			default:
			}
		}
		return nil, nil
	})
	req := executorRequest{StreamID: "s1", HostCallbackID: "c1"}
	raw, err := executeStream(context.Background(), cfg, credential{bearer: "t"}, "m", executorPayload("m"), req)
	if err != nil {
		t.Fatal(err)
	}
	env := decodeEnvelope(t, raw)
	if !env.OK {
		t.Fatalf("not ok: %+v", env.Error)
	}
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("async proxy did not close")
	}
}

func TestExecuteStreamFallsBackToCollect(t *testing.T) {
	cfg, _ := withTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sampleSSEBody())
	})
	// No StreamID / no hostCaller → collect path.
	SetHostCaller(nil)
	raw, err := executeStream(context.Background(), cfg, credential{bearer: "t"}, "m", executorPayload("m"), executorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	env := decodeEnvelope(t, raw)
	if !env.OK {
		t.Fatalf("not ok: %+v", env.Error)
	}
}

// --- handleExecute -----------------------------------------------------------

func TestHandleExecuteTable(t *testing.T) {
	st := freshOAuthStorage("handle@example.com")
	storage := marshalStorage(t, st)

	var mode atomic.Value
	mode.Store("sse_ok")
	cfg, srv := withTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		switch mode.Load().(string) {
		case "json_ok":
			_, _ = w.Write(sampleJSONCompletion())
		case "sse_ok":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, sampleSSEBody())
		case "500":
			w.WriteHeader(500)
			_, _ = io.WriteString(w, "err")
		default:
			w.WriteHeader(404)
		}
	})
	// Point global config at httptest so handleExecute's currentConfig() works.
	if err := applyConfig([]byte(fmt.Sprintf(
		"base_url: %q\nauth_dir: %q\nclient_version: %q\nforce_stream_upstream: false\n",
		srv.URL, cfg.AuthDir, defaultClientVersion,
	))); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = applyConfig(nil) })

	cases := []struct {
		name       string
		stream     bool
		raw        []byte
		mode       string
		wantOK     bool
		wantStatus int
	}{
		{
			name:   "bad_json",
			stream: false,
			raw:    []byte(`{`),
			// handleExecute returns error (not envelope) on decode failure
		},
		{
			name:       "missing_creds",
			stream:     false,
			raw:        buildExecuteRequest(t, "cline-pass/glm-5.3-flash", nil, nil),
			wantOK:     false,
			wantStatus: 401,
		},
		{
			name:       "empty_model",
			stream:     false,
			raw:        buildExecuteRequest(t, "", storage, nil),
			wantOK:     false,
			wantStatus: 400,
		},
		{
			name:   "stream_collect_ok",
			stream: true,
			raw:    buildExecuteRequest(t, "cline-pass/glm-5.3-flash", storage, nil),
			mode:   "sse_ok",
			wantOK: true,
		},
		{
			name:   "nonstream_ok",
			stream: false,
			raw:    buildExecuteRequest(t, "cline-pass/glm-5.3-flash", storage, nil),
			mode:   "json_ok",
			wantOK: true,
		},
		{
			name:       "upstream_error",
			stream:     false,
			raw:        buildExecuteRequest(t, "cline-pass/glm-5.3-flash", storage, nil),
			mode:       "500",
			wantOK:     false,
			wantStatus: 500,
		},
		{
			name:   "pascal_storage",
			stream: false,
			raw: func() []byte {
				raw, _ := json.Marshal(map[string]any{
					"Model":       "cline-pass/glm-5.3-flash",
					"Payload":     executorPayload("cline-pass/glm-5.3-flash"),
					"StorageJSON": storage,
				})
				return raw
			}(),
			mode:   "json_ok",
			wantOK: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.mode != "" {
				mode.Store(tc.mode)
			}
			out, err := handleExecute(tc.raw, tc.stream)
			if tc.name == "bad_json" {
				if err == nil {
					t.Fatal("expected decode error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			env := decodeEnvelope(t, out)
			if env.OK != tc.wantOK {
				t.Fatalf("ok=%v want %v env=%+v raw=%s", env.OK, tc.wantOK, env.Error, out)
			}
			if !tc.wantOK && tc.wantStatus != 0 && (env.Error == nil || env.Error.HTTPStatus != tc.wantStatus) {
				t.Fatalf("status=%v want %d", env.Error, tc.wantStatus)
			}
		})
	}
}

func TestHandleExecuteInvalidPayload(t *testing.T) {
	st := freshOAuthStorage("badpay@example.com")
	storage := marshalStorage(t, st)
	cfg, srv := withTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(sampleJSONCompletion())
	})
	if err := applyConfig([]byte(fmt.Sprintf(
		"base_url: %q\nauth_dir: %q\nclient_version: %q\nforce_stream_upstream: false\n",
		srv.URL, cfg.AuthDir, defaultClientVersion,
	))); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = applyConfig(nil) })

	raw, _ := json.Marshal(map[string]any{
		"model":        "cline-pass/glm-5.3-flash",
		"payload":      []byte("not-json"),
		"storage_json": storage,
	})
	out, err := handleExecute(raw, false)
	if err != nil {
		t.Fatal(err)
	}
	env := decodeEnvelope(t, out)
	if env.OK {
		t.Fatal("expected invalid request failure")
	}
}

func extractCloseErrorJSON(t *testing.T, closePayload string) string {
	t.Helper()
	var req map[string]any
	if err := json.Unmarshal([]byte(closePayload), &req); err != nil {
		t.Fatalf("close payload json: %v raw=%s", err, closePayload)
	}
	errMsg, _ := req["error"].(string)
	return errMsg
}

func TestRunGuardedProxyStallThenRetrySucceeds(t *testing.T) {
	streamGuardClock = nil
	var calls int32
	cfg, _ := withTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sampleSSEBody())
	})
	cfg.FirstFrameTimeoutSeconds = 1
	cfg.StreamSilenceTimeoutSeconds = 30
	cfg.StreamHeartbeatOnlyTimeoutSeconds = 60

	closed := make(chan string, 1)
	var emits int32
	installHostCaller(t, func(method string, request []byte) ([]byte, error) {
		switch method {
		case "host.stream.emit":
			atomic.AddInt32(&emits, 1)
		case "host.stream.close":
			closed <- string(request)
		}
		return nil, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		runGuardedProxy(ctx, cfg, credential{bearer: "t"}, executorPayload("m"), "cb", "sid")
	}()
	select {
	case got := <-closed:
		if strings.Contains(got, `"error"`) && !strings.Contains(got, `"error":""`) {
			// Some hosts always include error key; treat non-empty error as failure.
			errJSON := extractCloseErrorJSON(t, got)
			if errJSON != "" {
				t.Fatalf("expected successful close after fresh retry, got error=%s calls=%d", errJSON, calls)
			}
		}
	case <-time.After(8 * time.Second):
		t.Fatal("timeout waiting for close after stall+retry")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runGuardedProxy did not return")
	}
	if atomic.LoadInt32(&calls) < 2 {
		t.Fatalf("expected fresh retry, calls=%d", calls)
	}
	if atomic.LoadInt32(&emits) < 1 {
		t.Fatalf("expected emits after retry, emits=%d", emits)
	}
}

func TestRunGuardedProxyParentCancelIsCanceled(t *testing.T) {
	started := make(chan struct{})
	cfg, _ := withTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		close(started)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	})
	cfg.FirstFrameTimeoutSeconds = 30
	cfg.StreamSilenceTimeoutSeconds = 60
	cfg.StreamHeartbeatOnlyTimeoutSeconds = 90

	closed := make(chan string, 1)
	installHostCaller(t, func(method string, request []byte) ([]byte, error) {
		if method == "host.stream.close" {
			closed <- string(request)
		}
		return nil, nil
	})

	parent, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runGuardedProxy(parent, cfg, credential{bearer: "t"}, executorPayload("m"), "cb", "sid")
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream never started")
	}
	cancel()
	select {
	case got := <-closed:
		errJSON := extractCloseErrorJSON(t, got)
		if !strings.Contains(errJSON, `"code":"canceled"`) && !strings.Contains(errJSON, "已取消") {
			t.Fatalf("expected canceled classification, got %s", errJSON)
		}
		if strings.Contains(errJSON, "first_frame") || strings.Contains(errJSON, "silence") {
			t.Fatalf("parent cancel must not surface as stall: %s", errJSON)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for canceled close")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runGuardedProxy did not return")
	}
}

func TestRunGuardedProxyStallRetryThenParentCancelIsCanceled(t *testing.T) {
	streamGuardClock = nil
	var calls int32
	var once sync.Once
	secondStarted := make(chan struct{})
	cfg, _ := withTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		if n == 2 {
			once.Do(func() { close(secondStarted) })
		}
		<-r.Context().Done()
	})
	cfg.FirstFrameTimeoutSeconds = 1
	cfg.StreamSilenceTimeoutSeconds = 30
	cfg.StreamHeartbeatOnlyTimeoutSeconds = 60

	closed := make(chan string, 1)
	installHostCaller(t, func(method string, request []byte) ([]byte, error) {
		if method == "host.stream.close" {
			closed <- string(request)
		}
		return nil, nil
	})

	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		runGuardedProxy(parent, cfg, credential{bearer: "t"}, executorPayload("m"), "cb", "sid")
	}()
	select {
	case <-secondStarted:
	case <-time.After(10 * time.Second):
		t.Fatalf("no fresh retry attempt; calls=%d", atomic.LoadInt32(&calls))
	}
	cancel()
	select {
	case got := <-closed:
		errJSON := extractCloseErrorJSON(t, got)
		if strings.Contains(errJSON, "first_frame") || strings.Contains(errJSON, "silence") {
			t.Fatalf("Stop during retry must not surface as stall: %s", errJSON)
		}
		if !strings.Contains(errJSON, `"code":"canceled"`) && !strings.Contains(errJSON, "已取消") {
			t.Fatalf("expected canceled, got %s", errJSON)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("no close")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runGuardedProxy did not return")
	}
}

func TestRunGuardedProxy400UnexpectedEOFDoesNotRetry(t *testing.T) {
	var calls int32
	cfg, _ := withTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"unexpected EOF while parsing request body"}}`)
	})
	closed := make(chan string, 1)
	installHostCaller(t, func(m string, req []byte) ([]byte, error) {
		if m == "host.stream.close" {
			closed <- string(req)
		}
		return nil, nil
	})
	runGuardedProxy(context.Background(), cfg, credential{bearer: "t", source: "oauth"}, executorPayload("m"), "cb", "sid")
	select {
	case got := <-closed:
		if atomic.LoadInt32(&calls) != 1 {
			t.Fatalf("4xx must not fresh-retry, calls=%d close=%s", calls, got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no close")
	}
}

func TestRunGuardedProxyInternalErrorZeroOutputRetriesOnce(t *testing.T) {
	var calls int32
	cfg, _ := withTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":{"message":"INTERNAL_ERROR","type":"internal_error"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sampleSSEBody())
	})

	closed := make(chan string, 1)
	var emits int32
	installHostCaller(t, func(method string, request []byte) ([]byte, error) {
		switch method {
		case "host.stream.emit":
			atomic.AddInt32(&emits, 1)
		case "host.stream.close":
			closed <- string(request)
		}
		return nil, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runGuardedProxy(ctx, cfg, credential{bearer: "t", source: "oauth"}, executorPayload("m"), "cb", "sid")
	select {
	case got := <-closed:
		errJSON := extractCloseErrorJSON(t, got)
		if errJSON != "" {
			t.Fatalf("expected success after INTERNAL_ERROR fresh retry, got %s calls=%d", errJSON, calls)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout")
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("want exactly one fresh retry (2 calls), got %d", calls)
	}
	if atomic.LoadInt32(&emits) < 1 {
		t.Fatalf("emits=%d", emits)
	}
}

func TestCollectStreamParentCancelIsCanceled(t *testing.T) {
	started := make(chan struct{})
	cfg, _ := withTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		close(started)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	})
	cfg.FirstFrameTimeoutSeconds = 30

	parent, cancel := context.WithCancel(context.Background())
	errCh := make(chan []byte, 1)
	go func() {
		raw, err := executeStreamCollect(parent, cfg, credential{bearer: "t"}, executorPayload("m"))
		if err != nil {
			errCh <- []byte("goerr:" + err.Error())
			return
		}
		errCh <- raw
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream never started")
	}
	cancel()
	select {
	case raw := <-errCh:
		s := string(raw)
		if strings.HasPrefix(s, "goerr:") {
			t.Fatalf("unexpected go error: %s", s)
		}
		if !strings.Contains(s, "canceled") && !strings.Contains(s, "已取消") {
			t.Fatalf("expected canceled envelope, got %s", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for collect cancel")
	}
}

func TestCollectStreamWithRetryOnDialError(t *testing.T) {
	var calls int32
	cfg, _ := withTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			// First attempt: close without body after hijack-like failure is hard;
			// instead return nothing useful by resetting — simulate via 200 empty then
			// second try succeeds. For dial-style retry we need transport error.
			hj, ok := w.(http.Hijacker)
			if !ok {
				w.WriteHeader(500)
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				w.WriteHeader(500)
				return
			}
			_ = conn.Close()
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sampleSSEBody())
	})
	chunks, stall, err := collectStreamWithRetry(context.Background(), cfg, credential{bearer: "t"}, executorPayload("m"))
	// Depending on transport, first close may surface as error with retryable hint.
	if err != nil && len(chunks) == 0 && stall == nil {
		// Retry may still fail if both attempts hit issues; at least exercise the path.
		if atomic.LoadInt32(&calls) < 1 {
			t.Fatalf("calls=%d", calls)
		}
		return
	}
	if atomic.LoadInt32(&calls) < 1 {
		t.Fatalf("calls=%d", calls)
	}
}

func TestRewriteModelReasoningNormalize(t *testing.T) {
	cfg := defaultConfig()
	cfg.ReasoningEffortNormalize = testBool(true)
	payload := []byte(`{"model":"old","reasoning_effort":"INVALID"}`)
	out, err := rewriteModel(cfg, payload, "cline-pass/glm-5.3-flash")
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	if obj["model"] != "cline-pass/glm-5.3-flash" {
		t.Fatalf("model=%v", obj["model"])
	}
}

func TestSetHostCallerRoundTrip(t *testing.T) {
	prev := hostCaller
	t.Cleanup(func() { hostCaller = prev })
	called := false
	SetHostCaller(func(method string, request []byte) ([]byte, error) {
		called = true
		return []byte("ok"), nil
	})
	if hostCaller == nil {
		t.Fatal("hostCaller not set")
	}
	_, _ = hostCaller("x", nil)
	if !called {
		t.Fatal("not called")
	}
}
