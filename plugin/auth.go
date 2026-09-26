package plugin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

func handleAuthParse(request []byte) ([]byte, error) {
	raw := authParseMaterial(request)
	if len(raw) == 0 {
		return okEnvelope(map[string]any{"Handled": false})
	}
	var stored clineOAuthStorage
	if err := json.Unmarshal(raw, &stored); err != nil {
		return okEnvelope(map[string]any{"Handled": false})
	}
	if !strings.EqualFold(strings.TrimSpace(stored.Type), ProviderKey) {
		return okEnvelope(map[string]any{"Handled": false})
	}
	hasKey := strings.TrimSpace(stored.APIKey) != ""
	hasOAuth := strings.TrimSpace(stored.RefreshToken) != "" || strings.TrimSpace(stored.AccessToken) != ""
	if !hasKey && !hasOAuth {
		return okEnvelope(map[string]any{"Handled": false})
	}
	label := credentialLabel(&stored, hasKey, hasOAuth)
	fileName := authParseFileName(request)
	if fileName == "" {
		fileName = credentialFileName(&stored)
	}

	// Anti-pollution: a key file (cline-key-*.json) must never be treated as OAuth,
	// even if stale/corrupted OAuth tokens exist in its JSON.
	isKeyFile := strings.HasPrefix(fileName, "cline-key-")
	if isKeyFile {
		hasOAuth = false
	}

	meta := map[string]any{
		"type": ProviderKey,
	}
	if e := strings.TrimSpace(stored.Email); isPlausibleEmail(e) {
		meta["email"] = e
	}
	if l := strings.TrimSpace(label); l != "" {
		meta["label"] = l
	}
	if hasKey {
		meta["api_key"] = stored.APIKey
	}
	if hasOAuth && !isKeyFile {
		meta["access_token"] = withWorkOSPrefix(stored.AccessToken)
		meta["refresh_token"] = stored.RefreshToken
		meta["expires_at"] = stored.ExpiresAt
		meta["account_id"] = stored.AccountID
	}
	for k, v := range stored.Metadata {
		meta[k] = v
	}
	if isKeyFile {
		delete(meta, "access_token")
		delete(meta, "refresh_token")
		delete(meta, "expires_at")
	}
	// Publish the host refresh contract on load, not only after the first
	// refresh: the schedule has to exist from the moment the auth file is read,
	// otherwise a host restart reintroduces the "wait for the token to expire"
	// behaviour this contract exists to remove.
	nextRefreshAfter := publishHostRefreshContract(currentConfig(), meta, &stored)

	// Derive schedule priority based on credential preference.
	// As long as the credential has OAuth tokens, it is treated as an OAuth credential.
	priorityVal := priorityForCredential(currentConfig().CredentialPreference, hasOAuth)

	attrs := extractRequestAttributes(request)
	attrs["priority"] = fmt.Sprintf("%d", priorityVal)
	if strings.TrimSpace(attrs["path"]) == "" {
		if p := authFilePath(currentConfig(), &stored); p != "" {
			attrs["path"] = p
			attrs["source"] = p
			attrs["source_backend"] = "file"
		}
	}
	meta["priority"] = priorityVal

	auth := map[string]any{
		"Provider":    ProviderKey,
		"ID":          fileName,
		"FileName":    fileName,
		"Label":       label,
		"StorageJSON": raw,
		"Metadata":    meta,
		"Attributes":  attrs,
	}
	if !nextRefreshAfter.IsZero() {
		// PascalCase on purpose: the host decodes this into the untagged
		// pluginapi.AuthData struct, so snake_case keys would be dropped.
		auth["NextRefreshAfter"] = nextRefreshAfter.UTC().Format(time.RFC3339)
	}
	return okEnvelope(map[string]any{"Handled": true, "Auth": auth})
}

func authParseMaterial(request []byte) []byte {
	var probe struct {
		StorageJSON    []byte `json:"storage_json"`
		RawJSON        []byte `json:"raw_json"`
		Content        []byte `json:"content"`
		Data           []byte `json:"data"`
		Raw            []byte `json:"raw"`
		StorageJSONCap []byte `json:"StorageJSON"`
		RawJSONCap     []byte `json:"RawJSON"`
	}
	if err := json.Unmarshal(request, &probe); err == nil {
		for _, candidate := range [][]byte{
			probe.RawJSON, probe.RawJSONCap, probe.StorageJSON, probe.StorageJSONCap,
			probe.Content, probe.Data, probe.Raw,
		} {
			if len(candidate) > 0 {
				return candidate
			}
		}
	}
	var loose map[string]any
	if err := json.Unmarshal(request, &loose); err == nil {
		for _, k := range []string{"raw_json", "RawJSON", "storage_json", "StorageJSON", "content", "data", "raw"} {
			if v, ok := loose[k].(string); ok && v != "" {
				return []byte(v)
			}
		}
	}
	return nil
}

func clineAuthFileName(email string) string {
	email = strings.TrimSpace(email)
	if email == "" {
		return "cline.json"
	}
	return "cline-" + email + ".json"
}

// defaultAuthFileName is the generic, plugin-scoped name used when a credential
// carries no identity of its own. It never encodes where the credential runs
// ("nas", "mac") — the same file must be movable between hosts unchanged.
const defaultAuthFileName = "cline.json"

// credentialFileName is the file name handed to the host for a credential.
//
// The name is a path and the card subtitle, and it is what moves when a
// credential is copied between machines, so it is derived from the credential
// itself and never from the display label:
//
//	OAuth (has refresh/access token)  → cline-<email>.json  (historical, stable)
//	key-only                          → cline-key-<last4>.json
//	no identity at all                → cline.json
//
// The kind decides, not the email: a key-only credential learns its email from
// Cline (GET /users/me) but must keep the key slug — two keys on one account
// would otherwise collide on the same file.
func credentialFileName(st *clineOAuthStorage) string {
	if st == nil {
		return defaultAuthFileName
	}
	key := strings.TrimSpace(st.APIKey)
	oauth := strings.TrimSpace(st.RefreshToken) != "" || strings.TrimSpace(st.AccessToken) != ""
	email := strings.TrimSpace(st.Email)

	// An APIKey credential always resolves to its key slug
	if key != "" {
		return keyAuthFileName(key)
	}
	if oauth && email != "" {
		return clineAuthFileName(email)
	}
	if email != "" {
		return clineAuthFileName(email)
	}
	return defaultAuthFileName
}

// keyAuthFileName returns the canonical greek musical sequence file name for an API key.
//
// Read-only by contract: this sits on the auth.parse / auth.refresh path the
// host polls every 30s per credential, so it resolves the name and nothing else.
// Moving a legacy file is migrateLegacyKeyFiles' job, at config-apply time.
func keyAuthFileName(key string) string {
	dir := clineAuthDir(currentConfig())
	if dir != "" && isDirectory(dir) {
		name, err := resolveGreekKeyFileName(dir, key)
		if err == nil && name != "" {
			return name
		}
	}
	return "cline-key-" + lastN(key, 4) + ".json"
}

func keyAuthFileNameForDir(authDir, key string) string {
	if authDir != "" && isDirectory(authDir) {
		name, err := resolveGreekKeyFileName(authDir, key)
		if err == nil && name != "" {
			return name
		}
	}
	return "cline-key-" + lastN(key, 4) + ".json"
}

func authParseFileName(request []byte) string {
	var probe struct {
		FileName    string `json:"file_name"`
		Name        string `json:"name"`
		FileNameCap string `json:"FileName"`
	}
	_ = json.Unmarshal(request, &probe)
	if probe.FileName != "" {
		return probe.FileName
	}
	if probe.FileNameCap != "" {
		return probe.FileNameCap
	}
	return probe.Name
}

// credentialLabel decides the management-panel card title.
//
// Host rule (CLIProxyAPI internal/pluginhost/auth_provider.go:
// pluginAuthDataToCoreAuth): the card title is exactly AuthData.Label and the
// subtitle is AuthData.FileName. When no plugin claims a file, the host falls
// back to the file name with the "<provider>-" prefix stripped (which is why an
// unclaimed cline-nas-key.json shows as "nas-key"). So the title must carry the
// *identity*: the panel already renders "Cline" as the provider chip, and
// repeating the provider there wastes the one line that tells several accounts
// apart.
//
// Resolution order:
//
//  1. an explicit `label` in the credential metadata — a user override wins;
//  2. the account email (OAuth);
//  3. "API Key ····<last 4>" for key-only auths, so two keys never look alike;
//  4. the tier fallback.
//
// The subtitle stays the real file name: it is the routing identity users see in
// auth-files/config paths, so it is never prettified.
func credentialLabel(st *clineOAuthStorage, hasKey, hasOAuth bool) string {
	if st != nil {
		if l := stringFromMap(st.Metadata, "label", "Label", "display_name", "displayName"); l != "" {
			return l
		}
		if e := strings.TrimSpace(st.Email); e != "" {
			return e
		}
		if k := strings.TrimSpace(st.APIKey); k != "" {
			return "API Key ····" + lastN(k, 4)
		}
	}
	switch {
	case hasOAuth:
		return "Cline OAuth"
	case hasKey:
		return "Cline API Key"
	default:
		return "Cline OAuth"
	}
}

// lastN returns the last n runes of s (shorter strings are returned whole).
func lastN(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[len(r)-n:])
}

func handleAuthRefresh(request []byte) ([]byte, error) {
	raw := extractStorageJSON(request)
	if len(raw) == 0 {
		return ErrorEnvelope("invalid_request", "missing storage_json"), nil
	}
	var stored clineOAuthStorage
	if err := json.Unmarshal(unwrapJSONBytes(raw), &stored); err != nil {
		return ErrorEnvelope("invalid_request", "bad storage_json"), nil
	}
	cfg := currentConfig()
	requestAuthID := extractRequestAuthID(request)
	isKeyFile := strings.HasPrefix(requestAuthID, "cline-key-")

	// The host may have handed us a trimmed record (e.g. without expires_at).
	// Repair it from the auth file first: a missing expiry would otherwise make
	// every downstream freshness decision guesswork.
	if !isKeyFile {
		if repaired := withDiskFallback(cfg, &stored); repaired != nil {
			stored = *repaired
		}
	}
	if strings.TrimSpace(stored.RefreshToken) == "" || isKeyFile {
		// API-key-only auth: nothing to refresh. Ask Cline which account the key
		// belongs to (best effort) so the panel card can show the account instead
		// of a key hint, then echo the credential back.
		stored = enrichKeyOnlyIdentity(cfg, stored)
		label := credentialLabel(&stored, true, false)
		fileName := requestAuthID
		if fileName == "" {
			fileName = authParseFileName(request)
		}
		if fileName == "" {
			fileName = credentialFileName(&stored)
		}
		priorityVal := priorityForCredential(cfg.CredentialPreference, false)
		meta := map[string]any{
			"type":     ProviderKey,
			"api_key":  stored.APIKey,
			"priority": priorityVal,
		}
		if stored.Email != "" {
			meta["email"] = stored.Email
			if stored.AccountID != "" {
				meta["account_id"] = stored.AccountID
			}
		} else {
			meta["key_hint"] = "····" + lastN(stored.APIKey, 4)
		}
		attrs := extractRequestAttributes(request)
		attrs["priority"] = fmt.Sprintf("%d", priorityVal)
		if strings.TrimSpace(attrs["path"]) == "" {
			if p := authFilePath(cfg, &stored); p != "" {
				attrs["path"] = p
				attrs["source"] = p
				attrs["source_backend"] = "file"
			}
		}
		// An unresolved identity comes back soon instead of in a day: the lookup
		// may have failed transiently, and the card stays generic until it works.
		next := time.Now().Add(resolvedKeyRefresh)
		if stored.Email == "" {
			next = time.Now().Add(unresolvedKeyRetry)
		}
		auth := map[string]any{
			"Provider":    ProviderKey,
			"ID":          fileName,
			"FileName":    fileName,
			"Label":       label,
			"StorageJSON": raw,
			"Metadata":    meta,
			"Attributes":  attrs,
		}
		return okEnvelope(map[string]any{
			"Auth":             auth,
			"NextRefreshAfter": next.UTC().Format(time.RFC3339),
		})
	}
	// Same path as request-time refresh: single-flight + cross-process lock +
	// re-read, so the scheduled refresh never races a CLI/hub rotation.
	fresh, err := refreshOAuthIfNeeded(cfg, &stored, false)
	if err != nil {
		// A rejected refresh token must not take down a session whose access
		// token is still valid: keep serving and retry soon, so the user is only
		// asked to sign in again when the credential is genuinely unusable.
		accessStillValid := authStillUsable(&stored)
		if isReauthRequired(err) && !accessStillValid {
			// Genuine invalid_grant: the ONLY case that may surface as 401.
			logCredentialEvent(cfg, "auth.refresh: re-auth required account=%s err=%v",
				firstNonEmpty(stored.Email, stored.AccountID, diskProviderID), err)
			return ErrorEnvelopeRetryable("cline_reauth_required",
				"Cline 登录已失效（refresh token 被拒），请重新授权 Cline。", http.StatusUnauthorized, false), nil
		}
		if accessStillValid {
			logCredentialEvent(cfg, "auth.refresh failed (%v) but access token still valid for %dm — serving current credential, retrying shortly",
				err, minutesUntil(stored.ExpiresAt))
			fresh = &stored
		} else {
			// Transient failure (network/timeout/lock contention/5xx upstream) with
			// an expired token. Report 503 + retryable, never 401: the host treats
			// any 401 from a plugin refresh as "unauthorized" and then stops
			// refreshing this auth forever, which is precisely the outage this
			// status choice prevents.
			logCredentialEvent(cfg, "auth.refresh transient failure account=%s err=%v — reporting 503 retryable, keeping login",
				firstNonEmpty(stored.Email, stored.AccountID, diskProviderID), err)
			return ErrorEnvelopeRetryable("oauth_refresh_unavailable",
				"Cline 凭证暂时无法刷新（并非登录被拒），已保留登录态，将自动重试："+err.Error(),
				http.StatusServiceUnavailable, true), nil
		}
	}
	if fresh == nil {
		fresh = &stored
	}
	// A host poll that changed nothing means the schedule is working: the host
	// woke us early (per refresh_interval_seconds) and the token is still fresh,
	// so no upstream call was needed. Logged hourly, this is the cheapest proof
	// that the contract is honoured — and its absence would mean the host is back
	// to waiting for the token to expire.
	if fresh.ExpiresAt == stored.ExpiresAt && fresh.AccessToken == stored.AccessToken {
		logHostPollWithoutRefresh(cfg, &stored)
	}
	if fresh.Email == "" {
		fresh.Email = stored.Email
	}
	if fresh.AccountID == "" {
		fresh.AccountID = stored.AccountID
	}
	if fresh.APIKey == "" {
		fresh.APIKey = stored.APIKey
	}
	out, err := json.Marshal(fresh)
	if err != nil {
		return ErrorEnvelope("refresh_failed", err.Error()), nil
	}
	label := credentialLabel(fresh, false, true)
	fileName := extractRequestAuthID(request)
	if fileName == "" {
		fileName = authParseFileName(request)
	}
	if fileName == "" {
		fileName = credentialFileName(fresh)
	}
	priorityVal := priorityForCredential(cfg.CredentialPreference, true)
	meta := map[string]any{
		"type":          ProviderKey,
		"email":         label,
		"access_token":  fresh.AccessToken,
		"refresh_token": fresh.RefreshToken,
		"expires_at":    fresh.ExpiresAt,
		"account_id":    fresh.AccountID,
		"priority":      priorityVal,
	}
	attrs := extractRequestAttributes(request)
	attrs["priority"] = fmt.Sprintf("%d", priorityVal)
	if strings.TrimSpace(attrs["path"]) == "" {
		if p := authFilePath(cfg, fresh); p != "" {
			attrs["path"] = p
			attrs["source"] = p
			attrs["source_backend"] = "file"
		}
	}
	// Invalidate tier cache so a refreshed/changed token re-checks its plan if needed
	InvalidateTierCache(fresh.AccessToken)
	InvalidateTierCache(stored.AccessToken)

	// Re-publish the schedule on every refresh: the host overwrites its auth
	// metadata from this response, so a missing contract here would silently
	// downgrade the auth back to "refresh only after it breaks".
	next := publishHostRefreshContract(cfg, meta, fresh)
	if next.IsZero() {
		next = time.Now().Add(time.Minute).UTC()
	}
	auth := map[string]any{
		"Provider":    ProviderKey,
		"ID":          fileName,
		"FileName":    fileName,
		"Label":       label,
		"StorageJSON": out,
		"Metadata":    meta,
		"Attributes":  attrs,
	}
	return okEnvelope(map[string]any{
		"Auth":             auth,
		"NextRefreshAfter": next.UTC().Format(time.RFC3339),
	})
}

func extractStorageJSON(request []byte) []byte {
	var probe struct {
		StorageJSON    []byte `json:"storage_json"`
		StorageJSONCap []byte `json:"StorageJSON"`
		Auth           struct {
			StorageJSON []byte `json:"StorageJSON"`
			FileName    string `json:"FileName"`
		} `json:"Auth"`
	}
	_ = json.Unmarshal(request, &probe)
	if len(probe.StorageJSON) > 0 {
		return probe.StorageJSON
	}
	if len(probe.StorageJSONCap) > 0 {
		return probe.StorageJSONCap
	}
	if len(probe.Auth.StorageJSON) > 0 {
		return probe.Auth.StorageJSON
	}
	return authParseMaterial(request)
}

func extractRequestAttributes(request []byte) map[string]string {
	var probe struct {
		Attributes    map[string]string `json:"attributes"`
		AttributesCap map[string]string `json:"Attributes"`
		Auth          struct {
			Attributes map[string]string `json:"Attributes"`
		} `json:"Auth"`
	}
	_ = json.Unmarshal(request, &probe)
	out := make(map[string]string)
	for k, v := range probe.Attributes {
		if strings.TrimSpace(v) != "" {
			out[k] = v
		}
	}
	for k, v := range probe.AttributesCap {
		if strings.TrimSpace(v) != "" {
			out[k] = v
		}
	}
	for k, v := range probe.Auth.Attributes {
		if strings.TrimSpace(v) != "" {
			out[k] = v
		}
	}
	return out
}

func extractRequestAuthID(request []byte) string {
	var probe struct {
		AuthID      string `json:"AuthID"`
		AuthIDLow   string `json:"auth_id"`
		FileName    string `json:"file_name"`
		FileNameCap string `json:"FileName"`
		Name        string `json:"name"`
		Auth        struct {
			ID       string `json:"ID"`
			FileName string `json:"FileName"`
		} `json:"Auth"`
	}
	_ = json.Unmarshal(request, &probe)
	for _, candidate := range []string{probe.AuthID, probe.AuthIDLow, probe.FileName, probe.FileNameCap, probe.Name, probe.Auth.ID, probe.Auth.FileName} {
		if trimmed := strings.TrimSpace(candidate); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
