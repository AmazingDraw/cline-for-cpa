package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// A bare API key can still identify its account: Cline answers GET /users/me for
// an `sk_…` bearer and returns the email + display name of the user it bills
// (verified 2026-09-23 — the key resolved the same userId/email as the OAuth
// credential of the same account). So a key-only credential does not have to sit
// on the panel as "API Key ····fa21" forever.
//
// The lookup is best effort and never blocks a request path:
//
//   - it runs on the key-only auth.refresh branch (a control-plane path), not on
//     executor requests;
//   - the result is cached in-process and written into the auth file, so it
//     survives a host restart and is usually paid for exactly once;
//   - the file name is NOT derived from the resolved email (see
//     credentialFileName): two keys on one account would collide on one file.
//
// When the credential cannot be resolved the branch reports a short
// NextRefreshAfter, so the host comes back soon instead of in 24h.

type accountIdentity struct {
	Email       string
	DisplayName string
	UserID      string
}

const (
	accountCacheTTL     = 12 * time.Hour
	unresolvedKeyRetry  = 5 * time.Minute
	resolvedKeyRefresh  = 24 * time.Hour
	accountLookupBudget = 10 * time.Second
)

var (
	accountMu    sync.Mutex
	accountCache = map[string]cachedIdentity{}
)

// cachedIdentity keeps the lookup result only for accountCacheTTL, so an email
// change on Cline's side is picked up within a day instead of never.
type cachedIdentity struct {
	id accountIdentity
	at time.Time
}

// accountLookup is a package var so tests can stub the network call.
var accountLookup = func(cfg pluginConfig, bearer string) (*accountIdentity, error) {
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(cfg.BaseURL, "/")+"/users/me", nil)
	if err != nil {
		return nil, err
	}
	applyClineHeaders(req, cfg)
	req.Header.Set("Authorization", "Bearer "+bearer)

	budget := httpTimeout(cfg)
	if budget > accountLookupBudget {
		budget = accountLookupBudget
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	resp, err := upstreamClient().Do(req.WithContext(ctx))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("users/me HTTP %d: %s", resp.StatusCode, truncate(raw, 160))
	}
	var out struct {
		Success bool `json:"success"`
		Data    struct {
			ID          string `json:"id"`
			Email       string `json:"email"`
			DisplayName string `json:"displayName"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	id := accountIdentity{
		Email:       strings.TrimSpace(out.Data.Email),
		DisplayName: strings.TrimSpace(out.Data.DisplayName),
		UserID:      strings.TrimSpace(out.Data.ID),
	}
	if id.Email == "" {
		return nil, fmt.Errorf("users/me returned no email")
	}
	return &id, nil
}

func isPlausibleEmail(s string) bool {
	s = strings.TrimSpace(s)
	at := strings.Index(s, "@")
	if at <= 0 || at >= len(s)-1 {
		return false
	}
	dot := strings.LastIndex(s[at+1:], ".")
	return dot >= 0 && dot < len(s[at+1:])-1
}

// enrichKeyOnlyIdentity fills in the account identity for a key-only credential.
// Any failure leaves the credential exactly as it was.
func enrichKeyOnlyIdentity(cfg pluginConfig, st clineOAuthStorage) clineOAuthStorage {
	if isPlausibleEmail(st.Email) {
		return st
	}
	key := strings.TrimSpace(st.APIKey)
	if key == "" {
		return st
	}
	if cached, ok := cachedAccount(key); ok {
		return applyAccountIdentity(st, cached)
	}
	id, err := accountLookup(cfg, key)
	if err != nil {
		logCredentialEvent(cfg, "key-only identity lookup failed (card keeps the generic label): %v", err)
		return st
	}
	storeAccount(key, *id)
	st = applyAccountIdentity(st, *id)
	logCredentialEvent(cfg, "key-only identity resolved account=%s display=%q", id.Email, id.DisplayName)
	persistKeyOnlyIdentity(cfg, &st)
	return st
}

func applyAccountIdentity(st clineOAuthStorage, id accountIdentity) clineOAuthStorage {
	st.Email = id.Email
	if strings.TrimSpace(st.AccountID) == "" {
		st.AccountID = id.UserID
	}
	if st.Metadata == nil {
		st.Metadata = map[string]any{}
	}
	if id.DisplayName != "" {
		st.Metadata["name"] = id.DisplayName
	}
	return st
}

// persistKeyOnlyIdentity writes the resolved identity into the auth file so the
// card keeps showing it after a restart. It refuses to write when the file on
// disk is not named the way credentialFileName computes — creating a second file
// for the same credential would leave two ambiguous candidates behind.
func persistKeyOnlyIdentity(cfg pluginConfig, st *clineOAuthStorage) {
	path := authFilePath(cfg, st)
	if path == "" {
		return
	}
	if _, err := os.Stat(path); err != nil {
		logCredentialEvent(cfg, "key-only identity not persisted: expected auth file %q (rename the existing key file to keep the email on the card)",
			filepath.Base(path))
		return
	}
	if err := persistRefreshedStorage(cfg, st); err != nil {
		logCredentialEvent(cfg, "persist key-only identity failed: %v", err)
	}
}

func cachedAccount(key string) (accountIdentity, bool) {
	accountMu.Lock()
	defer accountMu.Unlock()
	entry, ok := accountCache[key]
	if !ok || time.Since(entry.at) > accountCacheTTL {
		return accountIdentity{}, false
	}
	return entry.id, true
}

func storeAccount(key string, id accountIdentity) {
	accountMu.Lock()
	defer accountMu.Unlock()
	if len(accountCache) > 64 {
		accountCache = map[string]cachedIdentity{}
	}
	accountCache[key] = cachedIdentity{id: id, at: time.Now()}
}
