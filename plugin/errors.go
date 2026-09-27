package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"

	"cline-for-cpa/plugin/streamguard"
)

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"http_status,omitempty"`
	// Retryable is advisory metadata for hosts/clients that understand it. The
	// CLIProxyAPI host currently classifies a plugin failure by HTTP status only
	// (sdk/cliproxy/auth/conductor_cooldown.go: isUnauthorizedError), so the
	// status we choose is what actually protects the auth from being poisoned;
	// this field keeps the intent explicit for everyone else.
	Retryable *bool `json:"retryable,omitempty"`
}

func okEnvelope(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

// ErrorEnvelope builds a failure envelope the host can surface to clients.
func ErrorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}

// ErrorEnvelopeWithStatus builds an error envelope with a specific HTTP status
// (cursor-for-cpa plugin/errors.go pattern).
func ErrorEnvelopeWithStatus(code, message string, status int) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{
		Code: code, Message: message, HTTPStatus: status,
	}})
	return raw
}

// ErrorEnvelopeRetryable builds an error envelope that advertises both an HTTP
// status and whether the same request may be retried unchanged.
//
// Use it for every recoverable credential failure: a 5xx status makes the host
// treat the auth as transiently broken (backoff + retry) instead of marking it
// unauthorized, which is what turns a one-second upstream hiccup into a
// permanent 503 that only a manual re-login clears.
func ErrorEnvelopeRetryable(code, message string, status int, retryable bool) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{
		Code: code, Message: message, HTTPStatus: status, Retryable: retryablePtr(retryable),
	}})
	return raw
}

// failure is everything a client should be told about a failed turn: HTTP
// status, Chinese (or stable) message, optional plugin-stable code, and
// retryability. Mirrors cursor-for-cpa failure + code field for stable codes.
type failure struct {
	status    int
	message   string
	code      string // plugin-stable when set; else openAIClassification
	retryable *bool
}

func retryablePtr(v bool) *bool { return &v }

// errorBody is the OpenAI-shaped document every client eventually parses.
type errorBody struct {
	Error errorBodyDetail `json:"error"`
}

type errorBodyDetail struct {
	Message   string `json:"message"`
	Type      string `json:"type"`
	Code      string `json:"code,omitempty"`
	Retryable *bool  `json:"retryable,omitempty"`
}

// openAIClassification mirrors the pair the host derives from a status
// (CLIProxyAPI sdk/api/handlers/handlers.go BuildErrorResponseBodyWithError).
func openAIClassification(status int) (string, string) {
	switch status {
	case http.StatusUnauthorized:
		return "authentication_error", "invalid_api_key"
	case http.StatusForbidden:
		return "permission_error", "insufficient_quota"
	case http.StatusTooManyRequests:
		return "rate_limit_error", "rate_limit_exceeded"
	case http.StatusNotFound:
		return "invalid_request_error", "model_not_found"
	default:
		if status >= http.StatusInternalServerError {
			return "server_error", "internal_server_error"
		}
		return "invalid_request_error", ""
	}
}

// errorBodyText renders a failure as the JSON document a client will read.
// Prefer plugin-stable f.code in error.code; otherwise host-style classification.
//
// Streaming turns already committed HTTP 200; host.stream.close only carries a
// string — packing this JSON into that string is the classification channel.
func errorBodyText(f failure) string {
	typ, hostCode := openAIClassification(f.status)
	code := f.code
	if code == "" {
		code = hostCode
	}
	raw, errMarshal := json.Marshal(errorBody{Error: errorBodyDetail{
		Message:   f.message,
		Type:      typ,
		Code:      code,
		Retryable: f.retryable,
	}})
	if errMarshal != nil {
		return f.message
	}
	return string(raw)
}

// FailureEnvelope wraps a classified failure for the non-stream / collect path.
// Envelope error.code prefers the plugin-stable code; message is the OpenAI JSON body.
func FailureEnvelope(f failure) []byte {
	code := f.code
	if code == "" {
		_, code = openAIClassification(f.status)
	}
	if code == "" {
		code = "request_error"
	}
	return ErrorEnvelopeWithStatus(code, errorBodyText(f), f.status)
}

// StallEnvelope maps a Stream Guard failure onto the classified envelope path.
func StallEnvelope(err *streamguard.StallError) []byte {
	return FailureEnvelope(ClassifyStall(err))
}

// ClassifyStall maps Stream Guard kinds to 504 + Chinese + retryable verdict.
func ClassifyStall(err *streamguard.StallError) failure {
	if err == nil {
		return failure{
			status:    http.StatusGatewayTimeout,
			message:   "上游流停滞：未知原因。",
			code:      "stream_stall",
			retryable: retryablePtr(true),
		}
	}
	switch err.Kind {
	case streamguard.KindFirstFrame:
		return failure{
			status:    http.StatusGatewayTimeout,
			message:   "上游首帧超时，可稍后重试。",
			code:      string(streamguard.KindFirstFrame),
			retryable: retryablePtr(true),
		}
	case streamguard.KindSilence:
		msg := "上游静默超时，可稍后重试。"
		if err.SilenceFor > 0 {
			msg = fmt.Sprintf("上游静默超时：约 %d 秒无入站帧，可稍后重试。", int(err.SilenceFor.Seconds()))
		}
		return failure{
			status:    http.StatusGatewayTimeout,
			message:   msg,
			code:      string(streamguard.KindSilence),
			retryable: retryablePtr(true),
		}
	case streamguard.KindHeartbeatOnly:
		msg := "上游心跳空转超时：仅有心跳无回合进展，重试多半无效。"
		if err.MeaningfulFor > 0 {
			msg = fmt.Sprintf("上游心跳空转超时：约 %d 秒仅有心跳无回合进展，重试多半无效。", int(err.MeaningfulFor.Seconds()))
		}
		return failure{
			status:    http.StatusGatewayTimeout,
			message:   msg,
			code:      string(streamguard.KindHeartbeatOnly),
			retryable: retryablePtr(false),
		}
	default:
		msg := err.Message
		if msg == "" {
			msg = "上游流停滞。"
		}
		return failure{
			status:    http.StatusGatewayTimeout,
			message:   msg,
			code:      string(err.Kind),
			retryable: retryablePtr(true),
		}
	}
}

// ClassifyUpstreamHTTP maps an upstream OpenAI-compatible HTTP status + body.
func ClassifyUpstreamHTTP(status int, body string) failure {
	body = strings.TrimSpace(body)
	// Cline reports tier limits in the message rather than in the status — the
	// official client classifies by these substrings too (see @cline/llms
	// isClineFreeModelLimitMessage / extractClineFreeModelLimitResetTime), so the
	// body gets a look before the status decides.
	if f, ok := classifyClineTierLimit(body); ok {
		return f
	}
	retryable := retryablePtr(false)
	switch status {
	case http.StatusTooManyRequests, http.StatusServiceUnavailable:
		retryable = retryablePtr(true)
	}

	var message string
	switch status {
	case http.StatusUnauthorized:
		message = "上游鉴权失败（401）：API Key 无效或已过期。"
	case http.StatusPaymentRequired:
		message = "上游要求付费或额度不足（402）。"
	case http.StatusForbidden:
		message = "上游拒绝访问（403）：无权使用该模型或账号受限。"
	case http.StatusTooManyRequests:
		message = "上游限流（429）：请求过于频繁，可稍后重试。"
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		message = fmt.Sprintf("上游服务异常（%d），可稍后重试。", status)
	default:
		if status >= 500 {
			message = fmt.Sprintf("上游服务器错误（%d）。", status)
		} else if status >= 400 {
			message = fmt.Sprintf("上游拒绝请求（%d）。", status)
		} else {
			message = fmt.Sprintf("上游返回异常状态（%d）。", status)
		}
	}
	if body != "" {
		message = message + " " + body
	}

	return failure{
		status:    status,
		message:   message,
		retryable: retryable,
		// code left empty → openAIClassification fills host-style code
	}
}

// Cline tier-limit markers. Verified against @cline/llms, which classifies the
// same strings: "free limit reached on model" → ClineFreeModelLimitError,
// "you have reached your clinepass limit" → ClinePassLimitError.
const (
	clineFreeLimitMarker = "free limit reached on model"
	clinePassLimitMarker = "clinepass limit"
	clineResetMarker     = "try again in "
)

// classifyClineTierLimit turns a tier-limit body into a first-class 429 so the
// caller sees which pool ran dry and when it refills, instead of a generic 4xx.
//
// Both limits are per-model and time-windowed server-side, so the reset hint is
// passed through verbatim — only Cline knows the window, and inventing one would
// be worse than showing theirs.
func classifyClineTierLimit(body string) (failure, bool) {
	if body == "" {
		return failure{}, false
	}
	lower := strings.ToLower(body)
	switch {
	case strings.Contains(lower, clineFreeLimitMarker):
		return failure{
			status:    http.StatusTooManyRequests,
			message:   "免费模型额度已用尽" + clineResetHint(body) + "。",
			code:      "cline_free_limit",
			retryable: retryablePtr(true),
		}, true
	case strings.Contains(lower, clinePassLimitMarker):
		return failure{
			status:    http.StatusTooManyRequests,
			message:   "ClinePass 订阅额度已用尽" + clineResetHint(body) + "。",
			code:      "cline_pass_limit",
			retryable: retryablePtr(true),
		}, true
	}
	return failure{}, false
}

// clineResetHint extracts the text after "try again in " — the same rule the
// official client uses. The tail is cut at the first quote or newline so JSON
// punctuation never leaks into the message.
func clineResetHint(body string) string {
	i := strings.Index(strings.ToLower(body), clineResetMarker)
	if i < 0 {
		return ""
	}
	rest := strings.TrimSpace(body[i+len(clineResetMarker):])
	if j := strings.IndexAny(rest, "\"\n"); j >= 0 {
		rest = strings.TrimSpace(rest[:j])
	}
	if rest == "" {
		return ""
	}
	return "，预计 " + truncate([]byte(rest), 60) + " 后恢复"
}

// upstreamHTTPError is returned by proxySSE when the upstream responds ≥400
// before the SSE body; ClassifyTransport unwraps it.
type upstreamHTTPError struct {
	Status int
	Body   string
}

func (e *upstreamHTTPError) Error() string {
	if e == nil {
		return "upstream http error"
	}
	if e.Body == "" {
		return fmt.Sprintf("upstream status %d", e.Status)
	}
	return fmt.Sprintf("upstream status %d: %s", e.Status, e.Body)
}

// ClassifyTransport maps dial / timeout / stream read / embedded upstream HTTP errors.
func ClassifyTransport(err error) failure {
	if err == nil {
		return failure{
			status:    http.StatusBadGateway,
			message:   "上游传输错误：未知原因。",
			code:      "upstream_stream_error",
			retryable: retryablePtr(true),
		}
	}

	var httpErr *upstreamHTTPError
	if errors.As(err, &httpErr) && httpErr != nil {
		return ClassifyUpstreamHTTP(httpErr.Status, httpErr.Body)
	}
	// Fallback: parse "upstream status N: ..." from legacy fmt.Errorf strings.
	if st, body, ok := parseUpstreamStatusError(err.Error()); ok {
		return ClassifyUpstreamHTTP(st, body)
	}

	if errors.Is(err, context.Canceled) {
		return failure{
			status:    499,
			message:   "本轮请求已取消。",
			code:      "canceled",
			retryable: retryablePtr(false),
		}
	}
	if errors.Is(err, context.DeadlineExceeded) || isTimeoutErr(err) {
		return failure{
			status:    http.StatusGatewayTimeout,
			message:   "连接或读取上游超时，可稍后重试。",
			code:      "upstream_timeout",
			retryable: retryablePtr(true),
		}
	}
	if isDialErr(err) {
		return failure{
			status:    http.StatusBadGateway,
			message:   "无法连接上游服务，可稍后重试。",
			code:      "upstream_dial_error",
			retryable: retryablePtr(true),
		}
	}

	return failure{
		status:    http.StatusBadGateway,
		message:   "上游流传输失败：" + err.Error(),
		code:      "upstream_stream_error",
		retryable: retryablePtr(true),
	}
}

func parseUpstreamStatusError(s string) (int, string, bool) {
	const prefix = "upstream status "
	if !strings.HasPrefix(s, prefix) {
		return 0, "", false
	}
	rest := strings.TrimPrefix(s, prefix)
	codeStr, body, cut := strings.Cut(rest, ":")
	codeStr = strings.TrimSpace(codeStr)
	st, err := strconv.Atoi(codeStr)
	if err != nil || st < 100 {
		return 0, "", false
	}
	if cut {
		return st, strings.TrimSpace(body), true
	}
	return st, "", true
}

func isTimeoutErr(err error) bool {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "timeout") || strings.Contains(msg, "timed out") || strings.Contains(msg, "i/o timeout")
}

func isDialErr(err error) bool {
	var op *net.OpError
	if errors.As(err, &op) && op != nil && op.Op == "dial" {
		return true
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "no such host") ||
		strings.Contains(msg, "dial tcp") ||
		strings.Contains(msg, "connect: ")
}

// ClassifyUpstreamHTTPForSource classifies an upstream failure with knowledge of
// which credential was rejected, so a 401 is never blamed on the wrong one.
//
// Legacy entry point: it assumes the OAuth 401 already survived a forced refresh
// (i.e. a freshly minted token was rejected too) and therefore reports
// "re-authorization required". Callers that know whether recovery actually
// succeeded must use ClassifyUpstreamHTTPForSourceAfterRefresh instead —
// reporting an unrecovered OAuth 401 as authorization failure is exactly what
// poisons the host credential (see that function's comment).
func ClassifyUpstreamHTTPForSource(status int, body, credentialSource string) failure {
	return ClassifyUpstreamHTTPForSourceAfterRefresh(status, body, credentialSource, true)
}

// ClassifyUpstreamHTTPForSourceAfterRefresh classifies an upstream failure and
// encodes the result in the HTTP status, because that is the only signal the
// CLIProxyAPI host uses to decide the fate of a credential.
//
// Host behaviour this must respect (CLIProxyAPI sdk/cliproxy/auth):
//
//	conductor_cooldown.go: isUnauthorizedError() → true on any 401
//	conductor_refresh.go:  a 401-classified refresh/exec failure sets
//	                       Unavailable=true, Status=StatusError, and
//	                       hasUnauthorizedAuthFailure() then makes
//	                       shouldRefresh() permanently false
//	selector.go:           such an auth yields 503 auth_unavailable (not 429),
//	                       so it never self-heals
//
// Therefore an OAuth 401 is only honest when a *new* token was rejected as well
// (recovered == true → the subscription really is dead and the user really must
// sign in again). When the forced refresh could not produce a new token
// (recovered == false) the correct answer is a retryable 5xx: the host backs off
// and retries instead of condemning the account.
func ClassifyUpstreamHTTPForSourceAfterRefresh(status int, body, credentialSource string, recovered bool) failure {
	if status != http.StatusUnauthorized {
		return ClassifyUpstreamHTTP(status, body)
	}
	switch credentialSource {
	case "oauth", "oauth_stale":
		if recovered {
			return failure{
				status: http.StatusUnauthorized,
				message: "Cline 登录态已失效（订阅凭证被上游拒绝），请在管理面板重新授权，无需改动 API Key。" +
					detailSuffix(body),
				code:      "cline_reauth_required",
				retryable: retryablePtr(false),
			}
		}
		return oauthTransientFailure(body)
	default:
		return failure{
			status:    http.StatusUnauthorized,
			message:   "上游鉴权失败（401）：Cline API Key 无效或已过期，请更新插件配置的 api_key。" + detailSuffix(body),
			code:      "invalid_api_key",
			retryable: retryablePtr(false),
		}
	}
}

// oauthTransientFailure describes a subscription credential that is temporarily
// unusable: the bearer was rejected, but we could not mint a replacement right
// now (refresh call failed, was locked by another holder, or the store was
// briefly unreadable). The credential itself is untouched, so this must never be
// reported as 401 — see ClassifyUpstreamHTTPForSourceAfterRefresh.
func oauthTransientFailure(detail string) failure {
	return failure{
		status: http.StatusServiceUnavailable,
		message: "订阅凭证暂时不可用：本轮未换到新令牌（并非登录被拒），已保留登录态，" +
			"请稍后重试。" + detailSuffix(detail),
		code:      "oauth_refresh_unavailable",
		retryable: retryablePtr(true),
	}
}

func detailSuffix(body string) string {
	body = strings.TrimSpace(body)
	if body == "" {
		return ""
	}
	return " 上游返回：" + truncate([]byte(body), 200)
}

// Convenience constructors for local (non-upstream) failures.
func missingCredentialsFailure() failure {
	return failure{
		status:    http.StatusUnauthorized,
		message:   "缺少 Cline 凭证：请在管理面板登录 Cline，或在插件配置填写 api_keys。",
		code:      "missing_credentials",
		retryable: retryablePtr(false),
	}
}

// reauthRequiredFailure is raised when the refresh token was rejected
// (invalid_grant) — the only condition that asks the user to sign in again.
func reauthRequiredFailure() failure {
	return failure{
		status:    http.StatusUnauthorized,
		message:   "Cline 登录已失效（refresh token 被拒），请重新授权 Cline。",
		code:      "cline_reauth_required",
		retryable: retryablePtr(false),
	}
}

func invalidModelFailure(detail string) failure {
	msg := "模型无效或不在服务命名空间内。"
	if detail != "" {
		msg = "模型无效：" + detail
	}
	return failure{
		status:    http.StatusBadRequest,
		message:   msg,
		code:      "invalid_model",
		retryable: retryablePtr(false),
	}
}

func invalidRequestFailure(detail string) failure {
	msg := "请求无效。"
	if detail != "" {
		msg = "请求无效：" + detail
	}
	return failure{
		status:    http.StatusBadRequest,
		message:   msg,
		code:      "invalid_request",
		retryable: retryablePtr(false),
	}
}
