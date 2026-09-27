package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	workosPrefix       = "workos:"
	defaultProvidersJS = ".cline/data/settings/providers.json"
	refreshPath        = "/auth/refresh"
	refreshLeadDefault = 5 * time.Minute
	// retryableTokenGraceDefault mirrors the official
	// DEFAULT_RETRYABLE_TOKEN_GRACE_MS (auth/cline.ts:44): a transient refresh
	// failure keeps using the current token while it is still fresh.
	retryableTokenGraceDefault = 30 * time.Second
)

// ErrReauthRequired means the refresh token was rejected: the account must sign
// in again. Mirrors the official OAuthReauthRequiredError contract — this is the
// ONLY condition that should tell a user to re-authenticate.
var ErrReauthRequired = errors.New("cline oauth re-authentication required")

// ErrNoCredentials means neither OAuth nor an API key is available for the
// selected auth. Distinct from a transient refresh failure: nothing can be
// retried until the user supplies a credential.
var ErrNoCredentials = errors.New("no api_key or oauth credentials")

var (
	invalidGrantCodePattern    = regexp.MustCompile(`(?i)invalid_grant|invalid_token|unauthorized`)
	invalidGrantMessagePattern = regexp.MustCompile(`(?i)invalid|expired|revoked|unauthorized`)
)

// clineTokenError mirrors the official ClineOAuthTokenError (auth/cline.ts:113-141).
type clineTokenError struct {
	status    int
	code      string
	requestID string
	message   string
}

func (e *clineTokenError) Error() string {
	if e == nil {
		return "cline token error"
	}
	return e.message
}

func (e *clineTokenError) isInvalidGrant() bool {
	if e == nil {
		return false
	}
	if e.code != "" && invalidGrantCodePattern.MatchString(e.code) {
		return true
	}
	if e.status == http.StatusBadRequest || e.status == http.StatusUnauthorized || e.status == http.StatusForbidden {
		return invalidGrantMessagePattern.MatchString(e.message)
	}
	return false
}

// isReauthRequired reports whether an error means the user must sign in again.
func isReauthRequired(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrReauthRequired) {
		return true
	}
	var te *clineTokenError
	return errors.As(err, &te) && te.isInvalidGrant()
}

// parseOAuthError extracts the OAuth error code and message from a refresh
// failure body so the invalid_grant vs. transient decision has real evidence.
func parseOAuthError(raw []byte) (code, message string) {
	var probe struct {
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
		Code             string `json:"code"`
		Message          string `json:"message"`
		Data             struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return "", strings.TrimSpace(truncate(raw, 200))
	}
	for _, candidate := range []string{probe.Error, probe.Code, probe.Data.Code} {
		if strings.TrimSpace(candidate) != "" {
			code = strings.TrimSpace(candidate)
			break
		}
	}
	for _, candidate := range []string{probe.ErrorDescription, probe.Message, probe.Data.Message} {
		if strings.TrimSpace(candidate) != "" {
			message = strings.TrimSpace(candidate)
			break
		}
	}
	if message == "" {
		message = code
	}
	return code, message
}

// clineOAuthStorage is the auth-file JSON we own (not the desktop providers.json).
type clineOAuthStorage struct {
	Type         string `json:"type"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    int64  `json:"expires_at"` // unix ms
	Email        string `json:"email,omitempty"`
	AccountID    string `json:"account_id,omitempty"`
	APIKey       string `json:"api_key,omitempty"` // fallback sk_ path
	// Disabled mirrors the auth file's panel toggle. The executor refuses to
	// run a disabled credential; see resolveCredentials.
	Disabled bool           `json:"disabled,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

type clineRefreshResponse struct {
	Success bool `json:"success"`
	Data    struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		TokenType    string `json:"tokenType"`
		ExpiresAt    string `json:"expiresAt"`
		UserInfo     struct {
			Email       string `json:"email"`
			Name        string `json:"name"`
			ClineUserID string `json:"clineUserId"`
			Subject     string `json:"subject"`
		} `json:"userInfo"`
	} `json:"data"`
}

func withWorkOSPrefix(token string) string {
	token = strings.TrimSpace(token)
	if token == "" {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(token), workosPrefix) {
		return token
	}
	return workosPrefix + token
}

func stripWorkOSPrefix(token string) string {
	token = strings.TrimSpace(token)
	if strings.HasPrefix(strings.ToLower(token), workosPrefix) {
		return token[len(workosPrefix):]
	}
	return token
}

func parseExpiresAtMs(iso string, fallback int64) int64 {
	if t, err := time.Parse(time.RFC3339, strings.TrimSpace(iso)); err == nil {
		return t.UnixMilli()
	}
	return fallback
}

func refreshClineOAuth(cfg pluginConfig, refreshToken string) (*clineOAuthStorage, error) {
	refreshToken = strings.TrimSpace(refreshToken)
	if refreshToken == "" {
		return nil, fmt.Errorf("empty refresh token")
	}
	base := strings.TrimRight(cfg.BaseURL, "/")
	if base == "" {
		base = defaultBaseURL
	}
	body, _ := json.Marshal(map[string]string{
		"refreshToken": refreshToken,
		"grantType":    "refresh_token",
	})
	req, err := http.NewRequest(http.MethodPost, base+refreshPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	applyClineHeaders(req, cfg)
	rctx, rcancel := context.WithTimeout(context.Background(), httpTimeout(cfg))
	defer rcancel()
	resp, err := upstreamClient().Do(req.WithContext(rctx))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		code, message := parseOAuthError(raw)
		return nil, &clineTokenError{
			status:    resp.StatusCode,
			code:      code,
			requestID: resp.Header.Get("x-request-id"),
			message:   fmt.Sprintf("token refresh failed: HTTP %d%s", resp.StatusCode, messageSuffix(message)),
		}
	}
	var parsed clineRefreshResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, err
	}
	if !parsed.Success || strings.TrimSpace(parsed.Data.AccessToken) == "" {
		return nil, fmt.Errorf("invalid refresh response")
	}
	rt := strings.TrimSpace(parsed.Data.RefreshToken)
	if rt == "" {
		rt = refreshToken
	}
	return &clineOAuthStorage{
		Type:         ProviderKey,
		AccessToken:  withWorkOSPrefix(parsed.Data.AccessToken),
		RefreshToken: rt,
		ExpiresAt:    parseExpiresAtMs(parsed.Data.ExpiresAt, time.Now().Add(55*time.Minute).UnixMilli()),
		Email:        strings.TrimSpace(parsed.Data.UserInfo.Email),
		AccountID:    strings.TrimSpace(parsed.Data.UserInfo.ClineUserID),
		Metadata: map[string]any{
			"provider":   "cline-pass",
			"token_type": parsed.Data.TokenType,
			"name":       parsed.Data.UserInfo.Name,
		},
	}, nil
}

func oauthNeedsRefresh(st *clineOAuthStorage, lead time.Duration) bool {
	if st == nil || strings.TrimSpace(st.RefreshToken) == "" {
		return false
	}
	if strings.TrimSpace(st.AccessToken) == "" {
		return true
	}
	if st.ExpiresAt <= 0 {
		return true
	}
	return time.Now().Add(lead).UnixMilli() >= st.ExpiresAt
}

// --- host refresh contract (0.2.8) ------------------------------------------
//
// The host schedules refreshes from two inputs we control:
//
//  1. Metadata["refresh_interval_seconds"] — read by
//     authPreferredInterval() → shouldRefresh(); a positive value gives the host
//     a reason to poll us for plugin providers that have no registered refresh
//     lead (cline is not in sdk/auth/refresh_registry.go).
//  2. Auth.NextRefreshAfter — the earliest time the host may try again; it is
//     honoured before the interval check (conductor_refresh.go:121).
//
// Both are published together on every auth.parse / auth.refresh so the schedule
// survives host restarts instead of living only in host memory.

// hostWakeAt is the earliest moment the host is asked to poll this credential.
//
// The value is deliberately earlier than the 5-minute lead we use internally
// (refreshLeadDefault) so the host gets a window to poll us before the token
// expires. It is NOT a safety margin against a coarse host loop: the host
// honours Auth.NextRefreshAfter exactly — its refresh loop is a per-auth heap
// timer (auto_refresh_loop.go resetTimer()/peek(), nextRefreshCheckAt()) with a
// 5s check interval (conductor_refresh.go refreshCheckInterval), not a fixed
// 15-minute grid. (An earlier revision of this comment asserted a 15-minute
// loop; checked against the host source on 2026-09-23, that was wrong — see
// docs/503根因与刷新契约.md.)
//
// Polling us early is free — refreshOAuthIfNeeded() answers with the current
// credential and makes no upstream call until the real lead is reached — but it
// is not unlimited: the host re-asks every 30s while a refresh is pending but
// has not yet produced a new token (conductor_refresh.go refreshIneffectiveBackoff),
// so a larger lead only buys more idle calls. That trade-off is why the default
// is 600s rather than the 30 minutes this wake point could in principle support.
func hostWakeAt(cfg pluginConfig, st *clineOAuthStorage) time.Time {
	if st == nil || st.ExpiresAt <= 0 {
		// Unknown expiry: report "no opinion" (zero) rather than a bogus time.
		return time.Time{}
	}
	lead := hostRefreshInterval(cfg)
	if lead <= 0 {
		// Contract disabled (`refresh_interval_seconds: 0`): keep the historical
		// 5-minute lead so nothing else changes.
		lead = refreshLeadDefault
	}
	wake := time.UnixMilli(st.ExpiresAt).Add(-lead)
	// A wake point in the past means "due now", which makes the host re-poll on
	// every loop. Floor it one minute out so early polls stay rare and cheap.
	if !wake.After(time.Now().Add(time.Minute)) {
		wake = time.Now().Add(time.Minute)
	}
	return wake.UTC()
}

// publishHostRefreshContract writes the interval into an auth Metadata bag and
// returns the NextRefreshAfter the caller should report to the host.
//
// The interval is published as a JSON number of seconds: the host's
// parseDurationValue() accepts float64/int/string, and JSON decoding of plugin
// metadata always yields float64, so a number is the safest wire form.
func publishHostRefreshContract(cfg pluginConfig, meta map[string]any, st *clineOAuthStorage) time.Time {
	if meta != nil {
		if d := hostRefreshInterval(cfg); d > 0 {
			meta["refresh_interval_seconds"] = int(d / time.Second)
		}
	}
	return hostWakeAt(cfg, st)
}

// --- credential recovery helpers (0.2.8) ------------------------------------

// withDiskFallback repairs a credential that the host passed down incompletely.
//
// The executor path receives credentials as StorageJSON plus a Metadata bag; if
// either is trimmed (or the host hands over only the access token), the plugin
// would believe it cannot refresh — oauthNeedsRefresh() returns false without a
// refresh token, the expired bearer is sent upstream, and a 401 follows. The
// auth file on disk is the source of truth, so fill the gaps from there before
// deciding anything. Never overwrites a value the host did supply.
func withDiskFallback(cfg pluginConfig, st *clineOAuthStorage) *clineOAuthStorage {
	if st == nil {
		return uniqueAuthFileCredential(cfg)
	}
	if strings.TrimSpace(st.RefreshToken) != "" && st.ExpiresAt > 0 {
		return st
	}
	onDisk := reloadStorageFromDisk(cfg, st)
	if onDisk == st || onDisk == nil {
		onDisk = uniqueAuthFileCredential(cfg)
	}
	if onDisk == nil {
		return st
	}
	merged := *st
	if strings.TrimSpace(merged.RefreshToken) == "" {
		merged.RefreshToken = onDisk.RefreshToken
	}
	if strings.TrimSpace(merged.AccessToken) == "" {
		merged.AccessToken = onDisk.AccessToken
	}
	if merged.ExpiresAt <= 0 {
		merged.ExpiresAt = onDisk.ExpiresAt
	}
	if strings.TrimSpace(merged.Email) == "" {
		merged.Email = onDisk.Email
	}
	if strings.TrimSpace(merged.AccountID) == "" {
		merged.AccountID = onDisk.AccountID
	}
	if merged.Type == "" {
		merged.Type = ProviderKey
	}
	// The disabled flag must also be recoverable from disk: the host hands over
	// trimmed records, and a toggle the operator set in the panel lives only in
	// the file. The field list above is deliberately explicit (a blind map
	// merge would resurrect fields the host intentionally omitted), so a new
	// storage field has to be added here too — this line is easy to forget,
	// which is exactly why the disabled-refusal test covers the disk-only case.
	if !merged.Disabled && onDisk.Disabled {
		merged.Disabled = true
	}
	return &merged
}

// uniqueAuthFileCredential returns the single OAuth credential stored under the
// plugin's auth dir, or nil when that would be ambiguous.
//
// Ambiguity is refused on purpose: picking one of several accounts by guesswork
// would spend the wrong subscription. Several candidates simply mean "no
// fallback", and the caller keeps whatever it already had.
func uniqueAuthFileCredential(cfg pluginConfig) *clineOAuthStorage {
	dir := clineAuthDir(cfg)
	if dir == "" {
		return nil
	}
	matches, err := filepath.Glob(filepath.Join(dir, "cline-*.json"))
	if err != nil || len(matches) != 1 {
		return nil
	}
	raw, err := os.ReadFile(matches[0])
	if err != nil {
		return nil
	}
	var st clineOAuthStorage
	if err := json.Unmarshal(unwrapJSONBytes(raw), &st); err != nil {
		return nil
	}
	if strings.TrimSpace(st.RefreshToken) == "" && strings.TrimSpace(st.AccessToken) == "" {
		return nil
	}
	if st.Type == "" {
		st.Type = ProviderKey
	}
	return &st
}

// --- single-flight (official runtime-oauth-token-manager.ts refreshInFlight) ---

type oauthRefreshCall struct {
	done   chan struct{}
	result oauthRefreshResult
}

type oauthRefreshResult struct {
	storage *clineOAuthStorage
	err     error
}

var (
	oauthRefreshMu       sync.Mutex
	oauthRefreshInFlight = map[string]*oauthRefreshCall{}
)

// credentialKey identifies a credential generation for coalescing purposes.
func credentialKey(st *clineOAuthStorage) string {
	if st == nil {
		return "cline"
	}
	return firstNonEmpty(st.AccountID, st.Email, diskProviderID)
}

// authStillUsable mirrors the official "transient failure keeps the current
// token" rule (auth/cline.ts DEFAULT_RETRYABLE_TOKEN_GRACE_MS = 30s): a
// credential is still worth using only while it has more than the grace left.
func authStillUsable(st *clineOAuthStorage) bool {
	if st == nil || strings.TrimSpace(st.AccessToken) == "" {
		return false
	}
	if st.ExpiresAt <= 0 {
		return false
	}
	return time.Until(time.UnixMilli(st.ExpiresAt)) > retryableTokenGraceDefault
}

// authSettingsEqual mirrors the official comparison (manager.ts:14-30):
// access + refresh + accountId + expiry together identify a credential
// generation. Used to decide both "someone else rotated it" and "it was
// replaced by a sign-out / new sign-in".
func authSettingsEqual(a, b *clineOAuthStorage) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.AccessToken == b.AccessToken &&
		a.RefreshToken == b.RefreshToken &&
		a.AccountID == b.AccountID &&
		a.ExpiresAt == b.ExpiresAt
}

// authReplacedOnDisk reports whether the credential on disk is a usable
// replacement for the snapshot the caller held — i.e. another holder (desktop
// app, CLI, hub) already rotated it and we may simply adopt their result instead
// of racing a second exchange.
//
// The check is deliberately strict, because the caller reached this path with a
// bearer the upstream has *just rejected*:
//
//   - the same bearer means disk has nothing newer → we must exchange for real;
//   - an empty or already-stale bearer is no replacement either — adopting it
//     would only move the 401 one step later;
//   - only a *different and still usable* bearer makes the exchange redundant.
//
// 0.2.7 compared four fields (access/refresh/accountId/expiresAt) for
// inequality, so a benign difference — a re-serialised millisecond expiry, a
// stale refresh token — suppressed the exchange and silently handed back the
// same dead bearer.
func authReplacedOnDisk(caller, disk *clineOAuthStorage) bool {
	if caller == nil || disk == nil {
		return false
	}
	diskBearer := strings.TrimSpace(disk.AccessToken)
	if diskBearer == "" || diskBearer == strings.TrimSpace(caller.AccessToken) {
		return false
	}
	return authStillUsable(disk)
}

// credentialFileExists reports whether the credential's auth file is locatable.
func credentialFileExists(cfg pluginConfig, st *clineOAuthStorage) bool {
	path := authFilePath(cfg, st)
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

// credentialChangedOnDisk reports whether the stored credential was replaced
// (or removed) while we were refreshing — the official code refuses to
// resurrect credentials that a sign-out or a new sign-in already replaced.
func credentialChangedOnDisk(cfg pluginConfig, baseline *clineOAuthStorage) bool {
	path := authFilePath(cfg, baseline)
	if path == "" {
		return false
	}
	if _, err := os.Stat(path); err != nil {
		return true // file gone (signed out) — do not resurrect
	}
	onDisk := reloadStorageFromDisk(cfg, baseline)
	if onDisk == baseline {
		return false
	}
	return !authSettingsEqual(baseline, onDisk)
}

// singleFlightRefresh collapses concurrent refreshes of the same credential into
// one HTTP call; followers wait for and reuse the leader's result.
func singleFlightRefresh(key string, fn func() (*clineOAuthStorage, error)) (*clineOAuthStorage, error) {
	oauthRefreshMu.Lock()
	if call, ok := oauthRefreshInFlight[key]; ok {
		oauthRefreshMu.Unlock()
		<-call.done
		return call.result.storage, call.result.err
	}
	call := &oauthRefreshCall{done: make(chan struct{})}
	oauthRefreshInFlight[key] = call
	oauthRefreshMu.Unlock()

	defer func() {
		oauthRefreshMu.Lock()
		delete(oauthRefreshInFlight, key)
		oauthRefreshMu.Unlock()
		close(call.done)
	}()
	call.result.storage, call.result.err = fn()
	return call.result.storage, call.result.err
}

// ensureFreshOAuth refreshes when the token is inside the 5-minute lead window.
func ensureFreshOAuth(cfg pluginConfig, st *clineOAuthStorage) (*clineOAuthStorage, error) {
	return refreshOAuthIfNeeded(cfg, st, false)
}

// refreshOAuthIfNeeded is the single path credentials are refreshed through:
// single-flight → cross-process lock → re-read → refresh → (caller saves).
func refreshOAuthIfNeeded(cfg pluginConfig, st *clineOAuthStorage, force bool) (*clineOAuthStorage, error) {
	if st == nil {
		return nil, fmt.Errorf("nil oauth storage")
	}
	if !force && !oauthNeedsRefresh(st, refreshLeadDefault) {
		return st, nil
	}
	fresh, err := singleFlightRefresh(credentialKey(st), func() (*clineOAuthStorage, error) {
		var out *clineOAuthStorage
		lockErr := withOAuthRefreshLock(cfg, func() error {
			current := reloadStorageFromDisk(cfg, st)
			// Another holder may already have rotated the credential while we
			// waited for the lock — reuse theirs instead of racing a rotation.
			if !force && !oauthNeedsRefresh(current, refreshLeadDefault) {
				out = current
				return nil
			}
			// A forced refresh (the upstream just rejected our bearer) may only be
			// skipped when disk holds a *usable replacement*: a different access
			// token that is still fresh. Any other difference — a stale refresh
			// token, a re-serialised expiry, a changed account id — must not turn
			// the forced exchange into a silent no-op; that is how a recoverable
			// 401 became a "dead login" verdict on 2026-09-23.
			if force && authReplacedOnDisk(st, current) {
				logCredentialEvent(cfg, "forced refresh: adopting the newer usable credential found on disk account=%s expires_in=%dm (bearer already rotated by another holder)",
					firstNonEmpty(current.Email, current.AccountID, diskProviderID), minutesUntil(current.ExpiresAt))
				out = current
				return nil
			}
			hadFile := credentialFileExists(cfg, current)
			refreshed, err := refreshClineOAuth(cfg, current.RefreshToken)
			if err != nil {
				return err
			}
			if refreshed.Email == "" {
				refreshed.Email = current.Email
			}
			if refreshed.AccountID == "" {
				refreshed.AccountID = current.AccountID
			}
			if refreshed.APIKey == "" {
				refreshed.APIKey = current.APIKey
			}
			if len(refreshed.Metadata) == 0 {
				refreshed.Metadata = current.Metadata
			}
			// A sign-out or a new sign-in that landed while we were refreshing
			// must win: never resurrect the credential it replaced (official
			// manager.ts:163-169 returns null in exactly this case).
			if hadFile && credentialChangedOnDisk(cfg, current) {
				logCredentialEvent(cfg, "WARN skip persist: credential replaced on disk during refresh (sign-out / new sign-in) account=%s",
					firstNonEmpty(current.Email, current.AccountID, diskProviderID))
				return fmt.Errorf("credential replaced during refresh")
			}
			// The rotated refresh token must reach disk, or the host's periodic
			// auth.refresh will present the dead one and force a manual re-auth.
			rotated := refreshed.RefreshToken != current.RefreshToken
			if perr := persistRefreshedStorage(cfg, refreshed); perr != nil {
				logCredentialEvent(cfg, "WARN refresh persisted FAILED (%v) account=%s expires_in=%dm rotated=%v",
					perr, firstNonEmpty(refreshed.Email, refreshed.AccountID, diskProviderID),
					minutesUntil(refreshed.ExpiresAt), rotated)
			} else {
				logCredentialEvent(cfg, "refreshed account=%s expires_in=%dm rotated=%v persisted=true",
					firstNonEmpty(refreshed.Email, refreshed.AccountID, diskProviderID),
					minutesUntil(refreshed.ExpiresAt), rotated)
			}
			out = refreshed
			return nil
		})
		if lockErr != nil {
			return nil, lockErr
		}
		return out, nil
	})
	if err != nil {
		if isReauthRequired(err) {
			logCredentialEvent(cfg, "refresh REJECTED (needs re-auth) account=%s err=%v",
				firstNonEmpty(st.Email, st.AccountID, diskProviderID), err)
			return nil, fmt.Errorf("%w: %v", ErrReauthRequired, err)
		}
		logCredentialEvent(cfg, "refresh transient failure account=%s err=%v",
			firstNonEmpty(st.Email, st.AccountID, diskProviderID), err)
		return nil, err
	}
	return fresh, nil
}

// minutesUntil renders a remaining lifetime for logs.
func minutesUntil(epochMs int64) int {
	if epochMs <= 0 {
		return 0
	}
	return int(time.Until(time.UnixMilli(epochMs)).Minutes())
}

func messageSuffix(message string) string {
	message = strings.TrimSpace(message)
	if message == "" {
		return ""
	}
	return ": " + message
}

// bootstrapFromProvidersJSON reads desktop providers.json once (read-only).
func bootstrapFromProvidersJSON(path string) (*clineOAuthStorage, error) {
	if path == "" {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, defaultProvidersJS)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Providers map[string]struct {
			TokenSource string `json:"tokenSource"`
			Settings    struct {
				Auth struct {
					AccessToken  string `json:"accessToken"`
					RefreshToken string `json:"refreshToken"`
					ExpiresAt    int64  `json:"expiresAt"`
					AccountID    string `json:"accountId"`
					Metadata     struct {
						Provider string `json:"provider"`
						UserInfo struct {
							Email string `json:"email"`
							Name  string `json:"name"`
						} `json:"userInfo"`
					} `json:"metadata"`
				} `json:"auth"`
			} `json:"settings"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	// Prefer cline-pass then cline.
	for _, key := range []string{"cline-pass", "cline"} {
		p, ok := doc.Providers[key]
		if !ok || strings.TrimSpace(p.Settings.Auth.RefreshToken) == "" {
			continue
		}
		a := p.Settings.Auth
		email := strings.TrimSpace(a.Metadata.UserInfo.Email)
		return &clineOAuthStorage{
			Type:         ProviderKey,
			AccessToken:  withWorkOSPrefix(a.AccessToken),
			RefreshToken: strings.TrimSpace(a.RefreshToken),
			ExpiresAt:    a.ExpiresAt,
			Email:        email,
			AccountID:    strings.TrimSpace(a.AccountID),
			Metadata: map[string]any{
				"provider":      firstNonEmpty(a.Metadata.Provider, key),
				"token_source":  p.TokenSource,
				"bootstrap":     true,
				"name":          a.Metadata.UserInfo.Name,
				"providers_key": key,
			},
		}, nil
	}
	return nil, fmt.Errorf("no cline/cline-pass refresh token in %s", path)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// oauthBearer returns the subscription (OAuth) bearer only.
func oauthBearer(st *clineOAuthStorage) string {
	if st == nil {
		return ""
	}
	return withWorkOSPrefix(st.AccessToken)
}

// bearerFromStorage prefers the OAuth token: an API key present in the same auth
// file must never silently hijack a subscription request into the usage-billing
// pool (the 402 "insufficient credits" failure mode).
func bearerFromStorage(st *clineOAuthStorage) string {
	if b := oauthBearer(st); b != "" {
		return b
	}
	if st != nil {
		return strings.TrimSpace(st.APIKey)
	}
	return ""
}

func parseStorageFromRequest(storageJSON []byte, metadata, authMetadata map[string]any) *clineOAuthStorage {
	if len(storageJSON) > 0 {
		var st clineOAuthStorage
		if err := json.Unmarshal(unwrapJSONBytes(storageJSON), &st); err == nil {
			if strings.EqualFold(strings.TrimSpace(st.Type), ProviderKey) || st.RefreshToken != "" || st.AccessToken != "" || st.APIKey != "" {
				if st.Type == "" {
					st.Type = ProviderKey
				}
				return &st
			}
		}
	}
	// Fallback: metadata bag from host. The host may omit StorageJSON entirely
	// (or hand over a trimmed one), so every field we need to decide "can this be
	// refreshed, and when" must be recoverable here — including expires_at,
	// which the 0.2.8 contract uses to schedule host-side polling.
	st := &clineOAuthStorage{Type: ProviderKey}
	st.APIKey = stringFromMap(metadata, "api_key", "APIKey", "apiKey")
	if st.APIKey == "" {
		st.APIKey = stringFromMap(authMetadata, "api_key", "APIKey", "apiKey")
	}
	st.AccessToken = stringFromMap(metadata, "access_token", "AccessToken")
	st.RefreshToken = stringFromMap(metadata, "refresh_token", "RefreshToken")
	st.Email = stringFromMap(metadata, "email", "Email")
	if st.Email == "" {
		st.Email = stringFromMap(authMetadata, "email", "Email")
	}
	st.AccountID = stringFromMap(metadata, "account_id", "accountId", "AccountID")
	st.ExpiresAt = int64FromMap(metadata, "expires_at", "expiresAt", "ExpiresAt")
	if st.ExpiresAt == 0 {
		st.ExpiresAt = int64FromMap(authMetadata, "expires_at", "expiresAt", "ExpiresAt")
	}
	if st.APIKey == "" && st.AccessToken == "" && st.RefreshToken == "" {
		return nil
	}
	return st
}

// int64FromMap reads the first present numeric key as a millisecond epoch.
// Metadata round-trips through JSON, so numbers arrive as float64 — and the
// host also stores expires_at as a string in some paths.
func int64FromMap(m map[string]any, keys ...string) int64 {
	if m == nil {
		return 0
	}
	for _, k := range keys {
		v, ok := m[k]
		if !ok {
			continue
		}
		switch t := v.(type) {
		case float64:
			if t > 0 {
				return int64(t)
			}
		case int64:
			if t > 0 {
				return t
			}
		case int:
			if t > 0 {
				return int64(t)
			}
		case json.Number:
			if i, err := t.Int64(); err == nil && i > 0 {
				return i
			}
		case string:
			if parsed, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64); err == nil && parsed > 0 {
				return parsed
			}
		}
	}
	return 0
}

// credential is the bearer resolved for one turn, plus provenance so failures
// can be explained instead of blamed on the wrong credential.
type credential struct {
	bearer  string
	source  string // oauth | oauth_stale | api_key_fallback | api_key
	oauth   bool
	storage *clineOAuthStorage
}

func resolveCredentials(cfg pluginConfig, req executorRequest) (credential, error) {
	st := parseStorageFromRequest(req.StorageJSON, req.Metadata, req.AuthMetadata)
	// The host may hand over a trimmed credential (e.g. access token only). Repair
	// it from the auth file before deciding whether a refresh is even possible —
	// otherwise an expired bearer is sent upstream and the turn ends in a 401 that
	// the host can mistake for a dead subscription.
	st = withDiskFallback(cfg, st)
	// A credential the host marked disabled must never be executed. The check
	// deliberately sits AFTER withDiskFallback: a trimmed record without the
	// flag is repaired from disk, and the on-disk disabled=true must still
	// stop it — checking before the repair would let exactly that record
	// through. (Routing should already skip disabled credentials; this is the
	// executor's last word before a bearer reaches the upstream, and for the
	// per-use billing key channel "the panel toggle is a lie" is not an
	// acceptable state.)
	if st != nil && st.Disabled {
		logThrottled(cfg, "disabled-credential:"+credentialKey(st), 5*time.Minute,
			"credential is disabled (%s) — refusing to execute it",
			firstNonEmpty(st.Email, st.APIKey, diskProviderID))
		return credential{}, ErrNoCredentials
	}
	// A credential that cannot be refreshed while its bearer is already stale is
	// the silent precursor of an upstream 401: warn (throttled) so the plugin side
	// leaves a trace. This exact silence is what made the 2026-09-23 outage look
	// like a dead login instead of an expired token.
	if st != nil && strings.TrimSpace(st.RefreshToken) == "" && oauthBearer(st) != "" && !authStillUsable(st) {
		logThrottled(cfg, "stale-bearer:"+credentialKey(st), 5*time.Minute,
			"credential has no refresh_token and its access token is stale (%dm left) — the request goes upstream with a stale bearer",
			minutesUntil(st.ExpiresAt))
	}
	// Prefer OAuth from the selected auth file (ClinePass subscription).
	if st != nil && (strings.TrimSpace(st.RefreshToken) != "" || strings.TrimSpace(st.AccessToken) != "") {
		fresh, err := ensureFreshOAuth(cfg, st)
		if err == nil && fresh != nil {
			if b := oauthBearer(fresh); b != "" {
				return credential{bearer: b, source: "oauth", oauth: true, storage: fresh}, nil
			}
		} else if err != nil {
			// Transient refresh failure with a still-valid token: keep using it
			// (official outcome=transient_failure_kept_current).
			if authStillUsable(st) {
				b := oauthBearer(st)
				pluginDebugf("oauth stale-but-valid (>30s left): continuing with current access token (%v)", err)
				return credential{bearer: b, source: "oauth_stale", oauth: true, storage: st}, nil
			}
			// Last resort: the usage-billing API key. Always reported — a silent
			// switch into the billing pool is what this plugin must never do.
			if k := fallbackAPIKey(st, cfg); k != "" {
				pluginLogf("OAuth unusable (%v) — falling back to configured api_key (usage-billing channel) for %s",
					err, firstNonEmpty(st.Email, st.AccountID, diskProviderID))
				return credential{bearer: k, source: "api_key_fallback", storage: st}, nil
			}
			if isReauthRequired(err) {
				return credential{}, fmt.Errorf("%w", ErrReauthRequired)
			}
			return credential{}, err
		}
	}
	if k := fallbackAPIKey(st, cfg); k != "" {
		return credential{bearer: k, source: "api_key", storage: st}, nil
	}
	return credential{}, ErrNoCredentials
}

// forceRefreshCredential force-refreshes an OAuth credential after an upstream
// 401 and reports whether the bearer actually changed (→ safe to retry).
func forceRefreshCredential(cfg pluginConfig, cred credential) (credential, bool) {
	if !cred.oauth || cred.storage == nil {
		return cred, false
	}
	fresh, err := refreshOAuthIfNeeded(cfg, cred.storage, true)
	if err != nil || fresh == nil {
		// Loud on purpose: this decision used to leave no trace at all, which is
		// precisely why the 2026-09-23 outage had to be reverse-engineered. The
		// request is about to fail a second time, so the reason must be on record.
		logCredentialEvent(cfg, "forced refresh produced no credential (err=%v) — surfacing the upstream rejection instead of retrying", err)
		return cred, false
	}
	b := oauthBearer(fresh)
	if b == "" || b == cred.bearer {
		logCredentialEvent(cfg, "forced refresh returned no new bearer — surfacing the upstream rejection instead of retrying (bearer unchanged)")
		return cred, false
	}
	pluginLogf("upstream 401 → forced refresh succeeded for %s",
		firstNonEmpty(fresh.Email, fresh.AccountID, diskProviderID))
	return credential{bearer: b, source: "oauth", oauth: true, storage: fresh}, true
}

// fallbackAPIKey returns the key borne by the credential the host handed over,
// and nothing else.
//
// It used to fall back to resolveAPIKey(cfg) — the plugin-level key from
// config.yaml — which is what made the panel's enable toggle a lie: the host
// could stop routing to a disabled key credential, but any OAuth hiccup sent
// this function digging out the config key anyway, silently billing the
// usage pool. That is exactly the switch the plugin's own comment at the
// oauth.go call site swears must never happen ("a silent switch into the
// billing pool is what this plugin must never do"). The config key now seeds
// a credential file (see syncConfigAPIKeyCredential) and the host routes;
// this function only reads what the host handed over.
func fallbackAPIKey(st *clineOAuthStorage, cfg pluginConfig) string {
	if st != nil {
		return strings.TrimSpace(st.APIKey)
	}
	return ""
}
