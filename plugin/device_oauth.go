package plugin

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Official Cline CLI/desktop WorkOS production client id
// (sdk/packages/shared/src/runtime/cline-environment.ts).
const (
	workOSClientIDProd = "client_01K3A541FN8TA3EPPHTD2325AR"
	workOSDeviceAuth   = "/user_management/authorize/device"
	workOSAuthenticate = "/user_management/authenticate"
	clineRegisterPath  = "/auth/register"
	deviceLoginTTL     = 10 * time.Minute
	deviceGrantType    = "urn:ietf:params:oauth:grant-type:device_code"
)

// workOSAPIBaseURL is overridable in tests (httptest) so device login never
// hits the public WorkOS API. Production default is unchanged.
var workOSAPIBaseURL = "https://api.workos.com"

type pendingDeviceLogin struct {
	deviceCode          string
	userCode            string
	verificationURI     string
	verificationURIComp string
	pollIntervalSec     int
	startedAt           time.Time
	expiresAt           time.Time
}

var (
	deviceLoginMu sync.Mutex
	deviceLogins  = map[string]*pendingDeviceLogin{}
)

type workOSDeviceAuthResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

type workOSTokenResponse struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	TokenType        string `json:"token_type"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

type clineRegisterResponse struct {
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

func forgetDeviceLogin(state string) {
	deviceLoginMu.Lock()
	delete(deviceLogins, state)
	deviceLoginMu.Unlock()
}

func pruneDeviceLogins() {
	now := time.Now()
	deviceLoginMu.Lock()
	defer deviceLoginMu.Unlock()
	for k, v := range deviceLogins {
		if v == nil || now.After(v.expiresAt) || now.Sub(v.startedAt) > deviceLoginTTL {
			delete(deviceLogins, k)
		}
	}
}

func requestWorkOSDeviceAuthorization(clientID string) (*workOSDeviceAuthResponse, error) {
	form := url.Values{}
	form.Set("client_id", clientID)
	req, err := http.NewRequest(http.MethodPost, workOSAPIBaseURL+workOSDeviceAuth, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := upstreamClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("WorkOS device auth HTTP %d: %s", resp.StatusCode, truncate(raw, 256))
	}
	var parsed workOSDeviceAuthResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, err
	}
	if strings.TrimSpace(parsed.DeviceCode) == "" || strings.TrimSpace(parsed.UserCode) == "" || strings.TrimSpace(parsed.VerificationURI) == "" {
		return nil, fmt.Errorf("invalid WorkOS device authorization response")
	}
	if parsed.ExpiresIn <= 0 {
		parsed.ExpiresIn = 300
	}
	if parsed.Interval <= 0 {
		parsed.Interval = 5
	}
	return &parsed, nil
}

// pollWorkOSDeviceOnce performs a single token poll (host drives the loop).
// Returns (nil, nil) while authorization is still pending.
func pollWorkOSDeviceOnce(clientID, deviceCode string) (access, refresh string, pending bool, err error) {
	form := url.Values{}
	form.Set("grant_type", deviceGrantType)
	form.Set("device_code", deviceCode)
	form.Set("client_id", clientID)
	req, err := http.NewRequest(http.MethodPost, workOSAPIBaseURL+workOSAuthenticate, strings.NewReader(form.Encode()))
	if err != nil {
		return "", "", false, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := upstreamClient().Do(req)
	if err != nil {
		return "", "", false, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var parsed workOSTokenResponse
	_ = json.Unmarshal(raw, &parsed)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if strings.TrimSpace(parsed.AccessToken) == "" || strings.TrimSpace(parsed.RefreshToken) == "" {
			return "", "", false, fmt.Errorf("invalid WorkOS token response")
		}
		return parsed.AccessToken, parsed.RefreshToken, false, nil
	}
	switch strings.TrimSpace(parsed.Error) {
	case "authorization_pending", "slow_down":
		return "", "", true, nil
	case "access_denied", "expired_token", "invalid_grant":
		msg := parsed.ErrorDescription
		if msg == "" {
			msg = parsed.Error
		}
		return "", "", false, fmt.Errorf("%s", msg)
	default:
		msg := parsed.ErrorDescription
		if msg == "" {
			msg = string(raw)
		}
		return "", "", false, fmt.Errorf("WorkOS authenticate HTTP %d: %s", resp.StatusCode, truncate([]byte(msg), 256))
	}
}

func registerClineWorkOSTokens(cfg pluginConfig, workosAccess, workosRefresh string) (*clineOAuthStorage, error) {
	base := strings.TrimRight(cfg.BaseURL, "/")
	if base == "" {
		base = defaultBaseURL
	}
	body, _ := json.Marshal(map[string]string{
		"accessToken":  workosAccess,
		"refreshToken": workosRefresh,
	})
	req, err := http.NewRequest(http.MethodPost, base+clineRegisterPath, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	applyClineHeaders(req, cfg)
	req.Header.Set("Accept", "application/json")
	resp, err := upstreamClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("register HTTP %d: %s", resp.StatusCode, truncate(raw, 256))
	}
	var parsed clineRegisterResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, err
	}
	if !parsed.Success || strings.TrimSpace(parsed.Data.AccessToken) == "" {
		return nil, fmt.Errorf("invalid register response")
	}
	rt := strings.TrimSpace(parsed.Data.RefreshToken)
	if rt == "" {
		return nil, fmt.Errorf("register response missing refresh token")
	}
	st := &clineOAuthStorage{
		Type:         ProviderKey,
		AccessToken:  withWorkOSPrefix(parsed.Data.AccessToken),
		RefreshToken: rt,
		ExpiresAt:    parseExpiresAtMs(parsed.Data.ExpiresAt, time.Now().Add(55*time.Minute).UnixMilli()),
		Email:        strings.TrimSpace(parsed.Data.UserInfo.Email),
		AccountID:    strings.TrimSpace(parsed.Data.UserInfo.ClineUserID),
		Metadata: map[string]any{
			"token_source": "device_oauth",
			"name":         strings.TrimSpace(parsed.Data.UserInfo.Name),
			"subject":      strings.TrimSpace(parsed.Data.UserInfo.Subject),
		},
	}
	return st, nil
}

func authMapFromOAuthStorage(st *clineOAuthStorage) (map[string]any, error) {
	if st == nil {
		return nil, fmt.Errorf("nil storage")
	}
	raw, err := json.Marshal(st)
	if err != nil {
		return nil, err
	}
	label := strings.TrimSpace(st.Email)
	if label == "" {
		label = "Cline OAuth"
	}
	fileName := clineAuthFileName(label)
	meta := map[string]any{
		"type":          ProviderKey,
		"email":         label,
		"access_token":  st.AccessToken,
		"refresh_token": st.RefreshToken,
		"expires_at":    st.ExpiresAt,
		"account_id":    st.AccountID,
	}
	for k, v := range st.Metadata {
		meta[k] = v
	}
	return map[string]any{
		"Provider":    ProviderKey,
		"ID":          fileName,
		"FileName":    fileName,
		"Label":       label,
		"StorageJSON": raw,
		"Metadata":    meta,
	}, nil
}

func handleAuthLoginStart(request []byte) ([]byte, error) {
	_ = request
	pruneDeviceLogins()
	dev, err := requestWorkOSDeviceAuthorization(workOSClientIDProd)
	if err != nil {
		return ErrorEnvelope("login_start_failed", err.Error()), nil
	}
	state := fmt.Sprintf("cline-device-%d", time.Now().UnixNano())
	expiresAt := time.Now().Add(time.Duration(dev.ExpiresIn) * time.Second)
	if time.Until(expiresAt) > deviceLoginTTL {
		expiresAt = time.Now().Add(deviceLoginTTL)
	}
	loginURL := strings.TrimSpace(dev.VerificationURIComplete)
	if loginURL == "" {
		loginURL = strings.TrimSpace(dev.VerificationURI)
	}
	deviceLoginMu.Lock()
	deviceLogins[state] = &pendingDeviceLogin{
		deviceCode:          dev.DeviceCode,
		userCode:            dev.UserCode,
		verificationURI:     dev.VerificationURI,
		verificationURIComp: dev.VerificationURIComplete,
		pollIntervalSec:     dev.Interval,
		startedAt:           time.Now(),
		expiresAt:           expiresAt,
	}
	deviceLoginMu.Unlock()

	return okEnvelope(map[string]any{
		"Provider":  ProviderKey,
		"URL":       loginURL,
		"State":     state,
		"ExpiresAt": expiresAt.UTC(),
		"Metadata": map[string]any{
			"user_code":             dev.UserCode,
			"verification_uri":      dev.VerificationURI,
			"poll_interval_seconds": dev.Interval,
			"instructions":          fmt.Sprintf("Enter this code in your browser: %s", dev.UserCode),
		},
	})
}

func handleAuthLoginPoll(request []byte) ([]byte, error) {
	var req struct {
		State         string `json:"State"`
		StateLower    string `json:"state"`
		Provider      string `json:"Provider"`
		ProviderLower string `json:"provider"`
	}
	_ = json.Unmarshal(request, &req)
	state := strings.TrimSpace(req.State)
	if state == "" {
		state = strings.TrimSpace(req.StateLower)
	}
	if state == "" {
		return okEnvelope(map[string]any{
			"Status":  "error",
			"Message": "missing login state",
		})
	}

	deviceLoginMu.Lock()
	entry := deviceLogins[state]
	deviceLoginMu.Unlock()
	if entry == nil {
		return okEnvelope(map[string]any{
			"Status":  "error",
			"Message": "unknown or expired Cline login state",
		})
	}
	if time.Now().After(entry.expiresAt) {
		forgetDeviceLogin(state)
		return okEnvelope(map[string]any{
			"Status":  "error",
			"Message": "Cline login timed out",
		})
	}

	access, refresh, pending, err := pollWorkOSDeviceOnce(workOSClientIDProd, entry.deviceCode)
	if err != nil {
		forgetDeviceLogin(state)
		return okEnvelope(map[string]any{
			"Status":  "error",
			"Message": "Cline login failed: " + err.Error(),
		})
	}
	if pending {
		return okEnvelope(map[string]any{
			"Status":  "pending",
			"Message": fmt.Sprintf("Waiting for browser confirmation (code %s)", entry.userCode),
		})
	}

	cfg := currentConfig()
	st, err := registerClineWorkOSTokens(cfg, access, refresh)
	if err != nil {
		forgetDeviceLogin(state)
		return okEnvelope(map[string]any{
			"Status":  "error",
			"Message": "Cline token registration failed: " + err.Error(),
		})
	}
	auth, err := authMapFromOAuthStorage(st)
	if err != nil {
		forgetDeviceLogin(state)
		return okEnvelope(map[string]any{
			"Status":  "error",
			"Message": err.Error(),
		})
	}
	forgetDeviceLogin(state)
	return okEnvelope(map[string]any{
		"Status": "success",
		"Auth":   auth,
	})
}
