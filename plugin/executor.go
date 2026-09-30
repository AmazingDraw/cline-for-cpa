package plugin

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"cline-for-cpa/plugin/streamguard"
)

type executorRequest struct {
	Model           string         `json:"model"`
	Payload         []byte         `json:"payload"` // host JSON-encodes []byte as base64
	StorageJSON     []byte         `json:"storage_json"`
	StorageJSONCap  []byte         `json:"StorageJSON"`
	Metadata        map[string]any `json:"metadata"`
	AuthMetadata    map[string]any `json:"auth_metadata"`
	AuthMetadataCap map[string]any `json:"AuthMetadata"`
	StreamID        string         `json:"stream_id"`
	HostCallbackID  string         `json:"host_callback_id"`
}

// normalizeCasing copies host ABI PascalCase fields the snake_case tags miss.
// encoding/json matches "Model" to `json:"model"` case-insensitively, but
// "StorageJSON" ≠ "storage_json" because the underscore survives folding.
func (r *executorRequest) normalizeCasing() {
	if len(r.StorageJSON) == 0 {
		r.StorageJSON = r.StorageJSONCap
	}
	if r.AuthMetadata == nil {
		r.AuthMetadata = r.AuthMetadataCap
	}
}

// hostCaller is set by main via SetHostCaller so stream chunks can be emitted.
var hostCaller func(method string, request []byte) ([]byte, error)

// SetHostCaller wires the C ABI host callback (tests may leave it nil).
func SetHostCaller(fn func(method string, request []byte) ([]byte, error)) {
	hostCaller = fn
}

// streamGuardClock is nil in production (streamguard.New falls back to RealClock).
// Tests may install a ManualClock so stall branches run without wall-clock waits.
var streamGuardClock streamguard.Clock

func handleExecute(request []byte, stream bool) ([]byte, error) {
	var req executorRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return nil, fmt.Errorf("decode executor request: %w", err)
	}
	req.normalizeCasing()
	cfg := currentConfig()
	cred, errCred := resolveCredentials(cfg, req)
	if errCred != nil {
		// The three outcomes must map to three different host-visible statuses:
		//   reauth required  → 401 (the only case that asks for a new sign-in)
		//   no credential    → 401 (nothing can be retried until one is added)
		//   refresh hiccup   → 503 retryable (the login is fine; retry later)
		// Mapping the last one to 401 is what made the host disable the auth
		// permanently, so it is deliberately handled separately.
		switch {
		case isReauthRequired(errCred):
			return FailureEnvelope(reauthRequiredFailure()), nil
		case errors.Is(errCred, ErrNoCredentials):
			return FailureEnvelope(missingCredentialsFailure()), nil
		default:
			logCredentialEvent(cfg, "credential unavailable for this turn (retryable, login kept): %v", errCred)
			return FailureEnvelope(oauthTransientFailure(errCred.Error())), nil
		}
	}
	if cred.bearer == "" {
		return FailureEnvelope(missingCredentialsFailure()), nil
	}
	clientModel, upstreamModel, err := NormalizeModel(req.Model)
	if err != nil {
		return FailureEnvelope(invalidModelFailure(err.Error())), nil
	}
	// The free and subscription tiers share model names and the upstream response
	// carries no tier fingerprint, so this line is the only production evidence of
	// which pool a request was routed into.
	logModelRoute(cfg, clientModel, upstreamModel)

	payload, err := rewriteModel(cfg, req.Payload, upstreamModel)
	if err != nil {
		return FailureEnvelope(invalidRequestFailure(err.Error())), nil
	}
	if stream {
		return executeStream(context.Background(), cfg, cred, upstreamModel, payload, req)
	}
	return executeOnce(context.Background(), cfg, cred, payload)
}

func rewriteModel(cfg pluginConfig, payload []byte, upstreamModel string) ([]byte, error) {
	payload = unwrapJSONBytes(payload)
	var obj map[string]any
	if err := json.Unmarshal(payload, &obj); err != nil {
		return nil, err
	}
	obj["model"] = upstreamModel
	if action, detail := normalizeReasoningEffortInPayload(obj, cfg); action != "" {
		logReasoningEffortEvent(cfg, action, detail)
	}
	return json.Marshal(obj)
}

// unwrapJSONBytes accepts raw object bytes or a JSON string that contains them.
func unwrapJSONBytes(raw []byte) []byte {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return raw
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return []byte(s)
		}
	}
	return raw
}

func stringFromMap(m map[string]any, keys ...string) string {
	if m == nil {
		return ""
	}
	for _, k := range keys {
		if v, ok := m[k]; ok {
			switch t := v.(type) {
			case string:
				if s := strings.TrimSpace(t); s != "" {
					return s
				}
			}
		}
	}
	return ""
}

func executeOnce(ctx context.Context, cfg pluginConfig, cred credential, payload []byte) ([]byte, error) {
	body, status, err := upstreamCompletion(ctx, cfg, cred, payload)
	if err != nil {
		return completionFailureEnvelope(err), nil
	}
	// Upstream 401 with a subscription credential: force one refresh and retry,
	// so an access token that expired mid-flight is invisible to the caller.
	//
	// refreshRecovered records whether a *new* bearer was obtained. It decides how
	// a remaining 401 is reported: a fresh token that is still rejected really is
	// a dead login (401 → re-authorize), while a 401 we could not recover from
	// must stay retryable so the host does not disable the credential.
	refreshRecovered := false
	if status == http.StatusUnauthorized {
		if newCred, changed := forceRefreshCredential(cfg, cred); changed {
			refreshRecovered = true
			body, status, err = upstreamCompletion(ctx, cfg, newCred, payload)
			if err != nil {
				return completionFailureEnvelope(err), nil
			}
			cred = newCred
		}
	}
	if status >= 400 {
		return FailureEnvelope(ClassifyUpstreamHTTPForSourceAfterRefresh(status, truncate(body, 512), cred.source, refreshRecovered)), nil
	}
	body = normalizeClineChatPayload(body)
	return okEnvelope(map[string]any{
		"Payload": body,
		"Headers": map[string][]string{"content-type": {"application/json"}},
	})
}

// streamStallError carries a Stream Guard stall out of the aggregated path.
type streamStallError struct{ stall *streamguard.StallError }

func (e *streamStallError) Error() string {
	if e == nil || e.stall == nil {
		return "stream stall"
	}
	return e.stall.Message
}

// upstreamCompletion serves a non-streaming client request. Cline only answers
// streaming requests, so unless disabled the call is made in streaming mode and
// the deltas are folded back into one chat.completion document.
func upstreamCompletion(ctx context.Context, cfg pluginConfig, cred credential, payload []byte) ([]byte, int, error) {
	if !forceStreamUpstream(cfg) {
		return upstreamOnce(ctx, cfg, cred, payload)
	}
	chunks, stall, err := collectStreamWithRetry(ctx, cfg, cred, payload)
	if stall != nil {
		return nil, 0, &streamStallError{stall: stall}
	}
	if err != nil {
		if status, body, ok := parseUpstreamStatusError(err.Error()); ok {
			return []byte(body), status, nil
		}
		return nil, 0, err
	}
	body, errAggregate := aggregateChunks(chunks)
	if errAggregate != nil {
		return nil, 0, errAggregate
	}
	return body, http.StatusOK, nil
}

// upstreamOnce performs exactly one upstream call and returns its raw outcome.
func upstreamOnce(ctx context.Context, cfg pluginConfig, cred credential, payload []byte) ([]byte, int, error) {
	httpReq, err := newUpstreamRequest(ctx, cfg, cred.bearer, payload, false)
	if err != nil {
		return nil, 0, err
	}
	resp, err := upstreamClient().Do(httpReq)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return body, resp.StatusCode, nil
}

func executeStream(ctx context.Context, cfg pluginConfig, cred credential, upstreamModel string, payload []byte, req executorRequest) ([]byte, error) {
	_ = upstreamModel

	// Async stream when host provides a stream id; otherwise collect synchronously.
	if req.StreamID == "" || hostCaller == nil {
		return executeStreamCollect(ctx, cfg, cred, payload)
	}

	// Derive a cancelable parent so the goroutine can be cleaned up on return.
	// Guard must NOT cancel this parent — only per-attempt child contexts — or a
	// stall poisons the fresh-retry path into a spurious "canceled".
	parent, cancel := context.WithCancel(ctx)
	go func() {
		defer cancel()
		runGuardedProxy(parent, cfg, cred, payload, req.HostCallbackID, req.StreamID)
	}()
	return okEnvelope(map[string]any{
		"headers": map[string][]string{"content-type": {"text/event-stream"}},
	})
}

// collectStreamOnce runs one streaming attempt and buffers its chunks.
func collectStreamOnce(ctx context.Context, cfg pluginConfig, cred credential, payload []byte) ([]map[string]any, *streamguard.StallError, error) {
	httpReq, err := newUpstreamRequest(ctx, cfg, cred.bearer, payload, true)
	if err != nil {
		return nil, nil, err
	}
	var (
		mu     sync.Mutex
		chunks []map[string]any
		stall  *streamguard.StallError
	)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// stall and chunks are both written by the Stream Guard's timer goroutine
	// and read by this one, so both live under mu. Guarding chunks alone left
	// stall racing: the read below is only ordered today by the happens-before
	// that cancel() happens to establish, which is luck rather than a contract.
	guard := streamguard.New(streamGuardConfig(cfg), streamGuardClock, func(e *streamguard.StallError) {
		mu.Lock()
		stall = e
		mu.Unlock()
		cancel()
	})
	guard.Start()
	defer guard.Disarm()

	err = proxySSE(ctx, cfg, httpReq, guard, func(line []byte) error {
		mu.Lock()
		chunks = append(chunks, map[string]any{"Payload": append([]byte(nil), line...)})
		mu.Unlock()
		return nil
	})
	mu.Lock()
	stallSnapshot := stall
	mu.Unlock()
	return chunks, stallSnapshot, err
}

// collectStreamWithRetry replays a stream attempt that produced nothing on a
// fresh connection (dead keep-alive socket or upstream stall).
func collectStreamWithRetry(parent context.Context, cfg pluginConfig, cred credential, payload []byte) ([]map[string]any, *streamguard.StallError, error) {
	chunks, stall, err := collectStreamOnce(parent, cfg, cred, payload)
	firstStall := stall
	if shouldRetryFreshConnection(stall, err, len(chunks) > 0) {
		if parent.Err() != nil {
			// True user Stop (or parent deadline): do not replay.
			return chunks, preferStall(firstStall, stall, err, parent), err
		}
		logCredentialEvent(cfg, "stream (collect) produced no output (stall=%v err=%v) — retrying on a fresh connection fresh_retry=1", stall != nil, err)
		dropIdleUpstreamConnections()
		chunks, stall, err = collectStreamOnce(parent, cfg, cred, payload)
	}
	return chunks, preferStall(firstStall, stall, err, parent), err
}

// preferStall picks which Guard stall to report after possibly multiple attempts.
// Parent cancel (user Stop / deadline) always wins: never report a stall as 504
// when the caller already canceled. Last-attempt stall otherwise wins. A
// first-round stall is kept only when a later attempt surfaces context.Canceled
// while parent is still alive (Guard poisoned the attempt ctx). A successful
// later attempt (err == nil) must not resurrect the first-round stall.
func preferStall(first, last *streamguard.StallError, err error, parent context.Context) *streamguard.StallError {
	if parent != nil && parent.Err() != nil {
		return nil
	}
	if last != nil {
		return last
	}
	if first == nil || err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return first
	}
	return nil
}

func executeStreamCollect(ctx context.Context, cfg pluginConfig, cred credential, payload []byte) ([]byte, error) {
	chunks, stall, err := collectStreamWithRetry(ctx, cfg, cred, payload)
	if err == nil && stall == nil {
		return okEnvelope(map[string]any{
			"headers": map[string][]string{"content-type": {"text/event-stream"}},
			"chunks":  chunks,
		})
	}
	// Nothing was emitted on a 401 → refresh once and retry the whole stream.
	// refreshRecovered tracks whether the retry ran with a freshly minted bearer,
	// which is what separates "subscription is dead" (401) from "refresh hiccup,
	// try again later" (503) in the classification below.
	refreshRecovered := false
	if status, ok := upstreamStatusOf(err); ok && status == http.StatusUnauthorized && stall == nil {
		if ctx.Err() == nil {
			if newCred, changed := forceRefreshCredential(cfg, cred); changed {
				refreshRecovered = true
				dropIdleUpstreamConnections()
				chunks, stall, err = collectStreamWithRetry(ctx, cfg, newCred, payload)
				if err == nil && stall == nil {
					return okEnvelope(map[string]any{
						"headers": map[string][]string{"content-type": {"text/event-stream"}},
						"chunks":  chunks,
					})
				}
				cred = newCred
			}
		}
	}
	// Outcome priority (aligned with runGuardedProxy):
	// 1) recorded stall  2) parent cancel without stall  3) HTTP  4) transport
	if stall != nil {
		return StallEnvelope(stall), nil
	}
	if ctx.Err() != nil {
		return FailureEnvelope(ClassifyTransport(context.Canceled)), nil
	}
	if err != nil {
		return FailureEnvelope(classifyProxyErr(err, cred, refreshRecovered)), nil
	}
	return okEnvelope(map[string]any{
		"headers": map[string][]string{"content-type": {"text/event-stream"}},
		"chunks":  chunks,
	})
}

func runGuardedProxy(parent context.Context, cfg pluginConfig, cred credential, payload []byte, callbackID, streamID string) {
	attempt := func(c credential) (*streamguard.StallError, error, bool) {
		// Each attempt gets its own cancelable child of parent. Guard may only
		// cancel this child; canceling parent would poison the next fresh retry.
		attemptCtx, attemptCancel := context.WithCancel(parent)
		defer attemptCancel()

		httpReq, err := newUpstreamRequest(attemptCtx, cfg, c.bearer, payload, true)
		if err != nil {
			return nil, err, false
		}
		var stall *streamguard.StallError
		// Same reasoning as collectStreamOnce: the guard fires on its timer
		// goroutine, so the handoff to this one is synchronised rather than
		// relying on cancel() to order it.
		var stallMu sync.Mutex
		guard := streamguard.New(streamGuardConfig(cfg), streamGuardClock, func(e *streamguard.StallError) {
			stallMu.Lock()
			stall = e
			stallMu.Unlock()
			attemptCancel()
		})
		guard.Start()
		defer guard.Disarm()

		emitted := false
		proxyErr := proxySSE(attemptCtx, cfg, httpReq, guard, func(line []byte) error {
			emitted = true
			return emitHostChunk(callbackID, streamID, line)
		})
		stallMu.Lock()
		stallSnapshot := stall
		stallMu.Unlock()
		return stallSnapshot, proxyErr, emitted
	}

	// refreshRecovered mirrors the collect path: true only when the replay used a
	// freshly minted bearer, so an unrecoverable 401 stays a retryable 503.
	refreshRecovered := false
	stall, err, emitted := attempt(cred)
	firstStall := stall
	// Retry once on a 401 that produced no output: refresh and replay.
	if err != nil && !emitted && stall == nil && parent.Err() == nil {
		if status, ok := upstreamStatusOf(err); ok && status == http.StatusUnauthorized {
			if newCred, changed := forceRefreshCredential(cfg, cred); changed {
				refreshRecovered = true
				dropIdleUpstreamConnections()
				cred = newCred
				stall, err, emitted = attempt(cred)
				if stall != nil {
					firstStall = stall
				}
			}
		}
	}
	// Replay once on a fresh connection when nothing was emitted: a silently
	// dead keep-alive socket is indistinguishable from an upstream stall here.
	if shouldRetryFreshConnection(stall, err, emitted) {
		if parent.Err() != nil {
			// User Stop (or parent deadline) — do not replay.
		} else {
			logCredentialEvent(cfg, "stream produced no output (stall=%v err=%v) — retrying on a fresh connection fresh_retry=1", stall != nil, err)
			dropIdleUpstreamConnections()
			stall, err, emitted = attempt(cred)
		}
	}
	reportStall := preferStall(firstStall, stall, err, parent)
	if reportStall != nil {
		// Primary path: OpenAI JSON in stream.close error string (HTTP already 200).
		closeHostStream(callbackID, streamID, errors.New(errorBodyText(ClassifyStall(reportStall))))
		return
	}
	if parent.Err() != nil {
		closeHostStream(callbackID, streamID, errors.New(errorBodyText(ClassifyTransport(context.Canceled))))
		return
	}
	if err != nil {
		closeHostStream(callbackID, streamID, errors.New(errorBodyText(classifyProxyErr(err, cred, refreshRecovered))))
		return
	}
	closeHostStream(callbackID, streamID, nil)
}

// shouldRetryFreshConnection reports whether an attempt that produced no output
// should be replayed on a brand-new connection. A silently dead keep-alive
// socket (dropped by NAT/VPN/proxy) looks exactly like an upstream stall; the
// request was never answered, so replaying is safe and restores the session
// instead of surfacing a 60s freeze to the user.
func shouldRetryFreshConnection(stall *streamguard.StallError, err error, emitted bool) bool {
	if emitted {
		return false
	}
	if stall != nil {
		return true
	}
	if err == nil {
		return false
	}
	// Mid-stream server_error with zero output is the official transient case
	// ("retry the full request"). Permanent codes and rate_limit must not be
	// replayed here — rate_limit needs a cooldown, and the error string contains
	// the substring "stream error", which the transport hints below would otherwise
	// treat as a fresh-connection failure.
	if me, ok := midStreamErrorOf(err); ok {
		return me.Code == "server_error"
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "mid-stream error") {
		return strings.Contains(msg, "(server_error)")
	}
	if status, ok := upstreamStatusOf(err); ok && status >= 400 && status < 500 {
		return false
	}
	// Zero-output transport / transient stream failures only. Keep hints narrow:
	// do not treat 401/4xx business errors as fresh-connection problems.
	for _, hint := range []string{
		"timeout", "timed out", "deadline",
		"connection reset", "eof", "broken pipe", "no route", "connection refused",
		"internal_error", "stream error", "http2:", "unexpected eof",
	} {
		if strings.Contains(msg, hint) {
			return true
		}
	}
	return false
}

// upstreamStatusOf extracts an HTTP status from a proxy/transport error.
func upstreamStatusOf(err error) (int, bool) {
	if err == nil {
		return 0, false
	}
	status, _, ok := parseUpstreamStatusError(err.Error())
	return status, ok
}

// midStreamProxyError is a finish_reason:"error" chunk observed on the streaming
// path. It is not an HTTP failure: the response was already 200.
type midStreamProxyError struct {
	Code      string
	Message   string
	RequestID string
}

func newMidStreamProxyError(d midStreamErrorDetail, requestID string) *midStreamProxyError {
	code := strings.TrimSpace(d.Code)
	if code == "" {
		code = "mid_stream_error"
	}
	msg := strings.TrimSpace(d.Message)
	if msg == "" {
		msg = "upstream error during generation"
	}
	return &midStreamProxyError{Code: code, Message: msg, RequestID: requestID}
}

func (e *midStreamProxyError) Error() string {
	if e == nil {
		return "upstream mid-stream error"
	}
	return fmt.Sprintf("upstream mid-stream error (%s): %s", e.Code, e.Message)
}

func midStreamErrorOf(err error) (*midStreamProxyError, bool) {
	var me *midStreamProxyError
	if errors.As(err, &me) && me != nil {
		return me, true
	}
	return nil, false
}

func requestIDLabel(id string) string {
	if strings.TrimSpace(id) == "" {
		return "(none)"
	}
	return id
}

// classifyProxyErr maps a proxySSE / transport error. Mid-stream codes win over
// the generic stream classifier so permanent failures stay retryable:false.
func classifyProxyErr(err error, cred credential, refreshRecovered bool) failure {
	if me, ok := midStreamErrorOf(err); ok {
		return classifyMidStream(me.Code, me.Message)
	}
	if status, ok := upstreamStatusOf(err); ok {
		return ClassifyUpstreamHTTPForSourceAfterRefresh(status, err.Error(), cred.source, refreshRecovered)
	}
	return ClassifyTransport(err)
}

// completionFailureEnvelope classifies a non-streaming upstreamCompletion error.
func completionFailureEnvelope(err error) []byte {
	var se *streamStallError
	if errors.As(err, &se) {
		return StallEnvelope(se.stall)
	}
	if me, ok := midStreamErrorOf(err); ok {
		return FailureEnvelope(classifyMidStream(me.Code, me.Message))
	}
	return FailureEnvelope(ClassifyTransport(err))
}

func proxySSE(ctx context.Context, cfg pluginConfig, httpReq *http.Request, guard *streamguard.Guard, emit func([]byte) error) error {
	httpReq = httpReq.WithContext(ctx)
	resp, err := upstreamClient().Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	reqID := strings.TrimSpace(resp.Header.Get("x-request-id"))
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		logCredentialEvent(cfg, "chat/completions upstream status %d x-request-id=%s", resp.StatusCode, requestIDLabel(reqID))
		return fmt.Errorf("upstream status %d: %s", resp.StatusCode, truncate(body, 512))
	}
	if reqID != "" {
		pluginDebugf("chat/completions x-request-id=%s", reqID)
	}
	reader := bufio.NewReader(resp.Body)
	var dataBuf bytes.Buffer

	flush := func() error {
		if dataBuf.Len() == 0 {
			return nil
		}
		data := append([]byte(nil), dataBuf.Bytes()...)
		dataBuf.Reset()
		guard.NoteInbound(ClassifySSEData(data))
		if detail := parseMidStreamError(data); detail != nil {
			me := newMidStreamProxyError(*detail, reqID)
			logCredentialEvent(cfg, "chat/completions mid-stream error code=%s x-request-id=%s", me.Code, requestIDLabel(reqID))
			// Do not emit the error chunk. Callers see a typed error and must not
			// close the stream as success. Zero-output server_error may still be
			// replayed once; anything already emitted is not.
			return me
		}
		// Host wraps each emit as "data: <payload>\n\n" — send raw JSON / [DONE] only.
		return emit(data)
	}

	for {
		line, errRead := reader.ReadBytes('\n')
		if len(line) > 0 {
			trimmed := bytes.TrimRight(line, "\r\n")
			switch {
			case IsSSEComment(trimmed):
				guard.NoteInbound(true)
			case bytes.HasPrefix(trimmed, []byte("data:")):
				// Cline emits one complete data line per event without a blank
				// separator. Flush any prior multi-line buffer, then emit this
				// line immediately when it already looks complete.
				if dataBuf.Len() > 0 {
					if err := flush(); err != nil {
						return err
					}
				}
				payload := bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:")))
				dataBuf.Write(payload)
				// Single-line events (JSON object or [DONE]) flush now.
				if len(payload) > 0 && (payload[0] == '{' || payload[0] == '[' || bytes.Equal(payload, []byte("[DONE]"))) {
					if err := flush(); err != nil {
						return err
					}
				}
			case len(trimmed) == 0:
				if err := flush(); err != nil {
					return err
				}
			default:
				// Ignore id:/event:/retry: framing for host; still counts as liveness.
				guard.NoteInbound(true)
			}
		}
		if errRead != nil {
			if errRead == io.EOF {
				if err := flush(); err != nil {
					return err
				}
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return errRead
		}
	}
}

func newUpstreamRequest(ctx context.Context, cfg pluginConfig, apiKey string, payload []byte, stream bool) (*http.Request, error) {
	base := strings.TrimRight(cfg.BaseURL, "/")
	url := base + "/chat/completions"
	body := payload
	if stream {
		var obj map[string]any
		if err := json.Unmarshal(payload, &obj); err != nil {
			return nil, err
		}
		obj["stream"] = true
		var err error
		body, err = json.Marshal(obj)
		if err != nil {
			return nil, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	applyClineHeaders(req, cfg)
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	return req, nil
}

func emitHostChunk(callbackID, streamID string, payload []byte) error {
	if hostCaller == nil {
		return nil
	}
	raw, _ := json.Marshal(map[string]any{
		"host_callback_id": callbackID,
		"stream_id":        streamID,
		"payload":          payload,
	})
	_, err := hostCaller("host.stream.emit", raw)
	return err
}

func closeHostStream(callbackID, streamID string, streamErr error) {
	if hostCaller == nil {
		return
	}
	req := map[string]any{
		"host_callback_id": callbackID,
		"stream_id":        streamID,
	}
	if streamErr != nil {
		req["error"] = streamErr.Error()
	}
	raw, _ := json.Marshal(req)
	_, _ = hostCaller("host.stream.close", raw)
}

// normalizeClineChatPayload unwraps Cline's {"success":true,"data":{...openai...}}
// envelope into a plain OpenAI chat.completion object for host/clients.
func normalizeClineChatPayload(body []byte) []byte {
	body = bytes.TrimSpace(body)
	if len(body) == 0 || body[0] != '{' {
		return body
	}
	var probe struct {
		Success *bool           `json:"success"`
		Data    json.RawMessage `json:"data"`
		Error   any             `json:"error"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return body
	}
	if probe.Success != nil && *probe.Success && len(probe.Data) > 0 && probe.Data[0] == '{' {
		return []byte(probe.Data)
	}
	return body
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}

// routeLogInterval throttles logModelRoute to one line per route per hour.
const routeLogInterval = time.Hour

var (
	routeLogMu   sync.Mutex
	routeLogSeen = map[string]time.Time{}
)

// logModelRoute records which upstream id a client-facing model resolved to,
// once per route per hour. cline-free/x and cline-pass/x carry identical model
// names and identical upstream response shapes, so without this line a silent
// namespace rewrite (free request billed to the subscription) would be invisible
// in production — which is exactly what 0.2.x did.
func logModelRoute(cfg pluginConfig, clientID, upstreamID string) {
	key := clientID + " → " + upstreamID
	now := time.Now()
	routeLogMu.Lock()
	last, seen := routeLogSeen[key]
	if seen && now.Sub(last) < routeLogInterval {
		routeLogMu.Unlock()
		return
	}
	routeLogSeen[key] = now
	routeLogMu.Unlock()
	logCredentialEvent(cfg, "route %s", key)
}
