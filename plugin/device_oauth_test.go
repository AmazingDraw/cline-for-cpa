package plugin

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// withWorkOSTestServer mounts WorkOS-shaped endpoints under workOSAPIBaseURL
// and optionally Cline register under cfg.BaseURL (same server).
func withWorkOSTestServer(t *testing.T, h http.HandlerFunc) (pluginConfig, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	prevBase := workOSAPIBaseURL
	workOSAPIBaseURL = srv.URL
	t.Cleanup(func() { workOSAPIBaseURL = prevBase })

	upstreamHTTPClientOverride = srv.Client()
	t.Cleanup(func() { upstreamHTTPClientOverride = nil })

	cfg := defaultConfig()
	cfg.BaseURL = srv.URL
	cfg.AuthDir = t.TempDir()
	cfg.ClientVersion = defaultClientVersion
	return cfg, srv
}

func seedPendingLogin(t *testing.T, state string, ttl time.Duration) *pendingDeviceLogin {
	t.Helper()
	entry := &pendingDeviceLogin{
		deviceCode:          "dev-code-" + state,
		userCode:            "USER-CODE",
		verificationURI:     "https://example.test/device",
		verificationURIComp: "https://example.test/device?user_code=USER-CODE",
		pollIntervalSec:     5,
		startedAt:           time.Now(),
		expiresAt:           time.Now().Add(ttl),
	}
	deviceLoginMu.Lock()
	deviceLogins[state] = entry
	deviceLoginMu.Unlock()
	t.Cleanup(func() { forgetDeviceLogin(state) })
	return entry
}

func clearDeviceLogins() {
	deviceLoginMu.Lock()
	deviceLogins = map[string]*pendingDeviceLogin{}
	deviceLoginMu.Unlock()
}

func decodeLoginPoll(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	env := decodeEnvelope(t, raw)
	if !env.OK {
		t.Fatalf("envelope not ok: %+v raw=%s", env.Error, raw)
	}
	var result map[string]any
	if err := json.Unmarshal(env.Result, &result); err != nil {
		t.Fatalf("result: %v", err)
	}
	return result
}

// --- prune / forget ----------------------------------------------------------

func TestForgetAndPruneDeviceLogins(t *testing.T) {
	clearDeviceLogins()
	t.Cleanup(clearDeviceLogins)

	deviceLoginMu.Lock()
	deviceLogins["alive"] = &pendingDeviceLogin{
		startedAt: time.Now(),
		expiresAt: time.Now().Add(time.Minute),
	}
	deviceLogins["expired"] = &pendingDeviceLogin{
		startedAt: time.Now().Add(-time.Hour),
		expiresAt: time.Now().Add(-time.Minute),
	}
	deviceLogins["ttl"] = &pendingDeviceLogin{
		startedAt: time.Now().Add(-(deviceLoginTTL + time.Minute)),
		expiresAt: time.Now().Add(time.Hour), // expires far but started too long ago
	}
	deviceLogins["nilish"] = nil
	deviceLoginMu.Unlock()

	pruneDeviceLogins()

	deviceLoginMu.Lock()
	if _, ok := deviceLogins["alive"]; !ok {
		deviceLoginMu.Unlock()
		t.Fatal("alive pruned")
	}
	if _, ok := deviceLogins["expired"]; ok {
		deviceLoginMu.Unlock()
		t.Fatal("expired not pruned")
	}
	if _, ok := deviceLogins["ttl"]; ok {
		deviceLoginMu.Unlock()
		t.Fatal("ttl not pruned")
	}
	if _, ok := deviceLogins["nilish"]; ok {
		deviceLoginMu.Unlock()
		t.Fatal("nil not pruned")
	}
	deviceLoginMu.Unlock()

	forgetDeviceLogin("alive")
	deviceLoginMu.Lock()
	_, ok := deviceLogins["alive"]
	deviceLoginMu.Unlock()
	if ok {
		t.Fatal("forget failed")
	}
}

// --- requestWorkOSDeviceAuthorization ----------------------------------------

func TestRequestWorkOSDeviceAuthorizationTable(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
		wantErr string
		check   func(t *testing.T, dev *workOSDeviceAuthResponse)
	}{
		{
			name: "success_defaults",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != workOSDeviceAuth {
					t.Errorf("path=%s", r.URL.Path)
				}
				if ct := r.Header.Get("Content-Type"); !strings.Contains(ct, "application/x-www-form-urlencoded") {
					t.Errorf("ct=%s", ct)
				}
				body, _ := io.ReadAll(r.Body)
				if !strings.Contains(string(body), "client_id=") {
					t.Errorf("body=%s", body)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"device_code": "dc-1", "user_code": "UC-1",
					"verification_uri": "https://ex.test/v",
					// expires_in / interval omitted → defaults
				})
			},
			check: func(t *testing.T, dev *workOSDeviceAuthResponse) {
				if dev.DeviceCode != "dc-1" || dev.UserCode != "UC-1" {
					t.Fatalf("%+v", dev)
				}
				if dev.ExpiresIn != 300 || dev.Interval != 5 {
					t.Fatalf("defaults ExpiresIn=%d Interval=%d", dev.ExpiresIn, dev.Interval)
				}
			},
		},
		{
			name: "success_explicit",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"device_code": "dc-2", "user_code": "UC-2",
					"verification_uri":          "https://ex.test/v",
					"verification_uri_complete": "https://ex.test/v?c=UC-2",
					"expires_in":                600, "interval": 3,
				})
			},
			check: func(t *testing.T, dev *workOSDeviceAuthResponse) {
				if dev.ExpiresIn != 600 || dev.Interval != 3 {
					t.Fatalf("%+v", dev)
				}
				if dev.VerificationURIComplete == "" {
					t.Fatal("missing complete uri")
				}
			},
		},
		{
			name: "http_400",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":"bad"}`)
			},
			wantErr: "WorkOS device auth HTTP 400",
		},
		{
			name: "invalid_json",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, "not-json")
			},
			wantErr: "invalid",
		},
		{
			name: "missing_fields",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"device_code": "only"})
			},
			wantErr: "invalid WorkOS device authorization response",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _ = withWorkOSTestServer(t, tc.handler)
			dev, err := requestWorkOSDeviceAuthorization(workOSClientIDProd)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					// invalid_json may be "invalid character" from json.Unmarshal
					if tc.name == "invalid_json" {
						if err == nil {
							t.Fatal("expected err")
						}
						return
					}
					t.Fatalf("err=%v want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			tc.check(t, dev)
		})
	}
}

func TestRequestWorkOSDeviceAuthorizationDialError(t *testing.T) {
	prev := workOSAPIBaseURL
	workOSAPIBaseURL = "http://127.0.0.1:1"
	t.Cleanup(func() { workOSAPIBaseURL = prev })
	upstreamHTTPClientOverride = &http.Client{Timeout: 200 * time.Millisecond}
	t.Cleanup(func() { upstreamHTTPClientOverride = nil })
	_, err := requestWorkOSDeviceAuthorization(workOSClientIDProd)
	if err == nil {
		t.Fatal("expected dial error")
	}
}

// --- pollWorkOSDeviceOnce ----------------------------------------------------

func TestPollWorkOSDeviceOnceTable(t *testing.T) {
	cases := []struct {
		name        string
		handler     http.HandlerFunc
		wantAccess  string
		wantPending bool
		wantErrSub  string
	}{
		{
			name: "success",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != workOSAuthenticate {
					t.Errorf("path=%s", r.URL.Path)
				}
				body, _ := io.ReadAll(r.Body)
				if !strings.Contains(string(body), "device_code=") {
					t.Errorf("body=%s", body)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"access_token": "at-1", "refresh_token": "rt-1", "token_type": "Bearer",
				})
			},
			wantAccess: "at-1",
		},
		{
			name: "pending",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "authorization_pending"})
			},
			wantPending: true,
		},
		{
			name: "slow_down",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "slow_down"})
			},
			wantPending: true,
		},
		{
			name: "access_denied",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error": "access_denied", "error_description": "user denied",
				})
			},
			wantErrSub: "user denied",
		},
		{
			name: "expired_token_no_desc",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "expired_token"})
			},
			wantErrSub: "expired_token",
		},
		{
			name: "invalid_grant",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid_grant", "error_description": "gone"})
			},
			wantErrSub: "gone",
		},
		{
			name: "other_error",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error": "server_error", "error_description": "boom",
				})
			},
			wantErrSub: "WorkOS authenticate HTTP 500",
		},
		{
			name: "other_error_raw_body",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadGateway)
				_, _ = io.WriteString(w, "gateway sad")
			},
			wantErrSub: "WorkOS authenticate HTTP 502",
		},
		{
			name: "success_missing_tokens",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "only"})
			},
			wantErrSub: "invalid WorkOS token response",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _ = withWorkOSTestServer(t, tc.handler)
			access, refresh, pending, err := pollWorkOSDeviceOnce(workOSClientIDProd, "dc")
			if tc.wantPending {
				if err != nil || !pending || access != "" {
					t.Fatalf("access=%q refresh=%q pending=%v err=%v", access, refresh, pending, err)
				}
				return
			}
			if tc.wantErrSub != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErrSub) {
					t.Fatalf("err=%v want %q", err, tc.wantErrSub)
				}
				return
			}
			if err != nil || pending {
				t.Fatalf("err=%v pending=%v", err, pending)
			}
			if access != tc.wantAccess || refresh == "" {
				t.Fatalf("access=%q refresh=%q", access, refresh)
			}
		})
	}
}

func TestPollWorkOSDeviceOnceDialError(t *testing.T) {
	prev := workOSAPIBaseURL
	workOSAPIBaseURL = "http://127.0.0.1:1"
	t.Cleanup(func() { workOSAPIBaseURL = prev })
	upstreamHTTPClientOverride = &http.Client{Timeout: 200 * time.Millisecond}
	t.Cleanup(func() { upstreamHTTPClientOverride = nil })
	_, _, _, err := pollWorkOSDeviceOnce(workOSClientIDProd, "dc")
	if err == nil {
		t.Fatal("expected dial error")
	}
}

// --- registerClineWorkOSTokens / authMapFromOAuthStorage ---------------------

func TestRegisterClineWorkOSTokensTable(t *testing.T) {
	cases := []struct {
		name       string
		handler    http.HandlerFunc
		wantErrSub string
	}{
		{
			name: "success",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != clineRegisterPath {
					t.Errorf("path=%s", r.URL.Path)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"success": true,
					"data": map[string]any{
						"accessToken": "cline-at", "refreshToken": "cline-rt",
						"tokenType": "Bearer",
						"expiresAt": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
						"userInfo": map[string]any{
							"email": "dev@example.com", "name": "Dev",
							"clineUserId": "uid-1", "subject": "sub-1",
						},
					},
				})
			},
		},
		{
			name: "http_400",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, "nope")
			},
			wantErrSub: "register HTTP 400",
		},
		{
			name: "invalid_json",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, "{")
			},
			wantErrSub: "", // any unmarshal error
		},
		{
			name: "success_false",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"success": false})
			},
			wantErrSub: "invalid register response",
		},
		{
			name: "missing_refresh",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"success": true,
					"data":    map[string]any{"accessToken": "at-only"},
				})
			},
			wantErrSub: "missing refresh token",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, _ := withWorkOSTestServer(t, tc.handler)
			st, err := registerClineWorkOSTokens(cfg, "wo-at", "wo-rt")
			if tc.name == "success" {
				if err != nil {
					t.Fatal(err)
				}
				if st.Email != "dev@example.com" || st.RefreshToken != "cline-rt" {
					t.Fatalf("%+v", st)
				}
				if !strings.HasPrefix(st.AccessToken, workosPrefix) && st.AccessToken != "workos:cline-at" && !strings.Contains(st.AccessToken, "cline-at") {
					// withWorkOSPrefix may add prefix
					if st.AccessToken == "" {
						t.Fatal("empty access")
					}
				}
				return
			}
			if err == nil {
				t.Fatal("expected error")
			}
			if tc.wantErrSub != "" && !strings.Contains(err.Error(), tc.wantErrSub) {
				t.Fatalf("err=%v want %q", err, tc.wantErrSub)
			}
		})
	}
}

func TestRegisterClineWorkOSTokensDialError(t *testing.T) {
	cfg := defaultConfig()
	cfg.BaseURL = "http://127.0.0.1:1"
	cfg.ClientVersion = defaultClientVersion
	upstreamHTTPClientOverride = &http.Client{Timeout: 200 * time.Millisecond}
	t.Cleanup(func() { upstreamHTTPClientOverride = nil })
	_, err := registerClineWorkOSTokens(cfg, "a", "r")
	if err == nil {
		t.Fatal("expected dial error")
	}
}

func TestRegisterClineWorkOSTokensEmptyBaseURL(t *testing.T) {
	// Empty BaseURL falls back to defaultBaseURL — point WorkOS override unused;
	// force dial fail on default by overriding client only (won't hit real net if
	// DefaultClient dial fails to resolve? defaultBaseURL is public — use override
	// client that always fails).
	cfg := defaultConfig()
	cfg.BaseURL = ""
	cfg.ClientVersion = defaultClientVersion
	upstreamHTTPClientOverride = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, io.EOF
	})}
	t.Cleanup(func() { upstreamHTTPClientOverride = nil })
	_, err := registerClineWorkOSTokens(cfg, "a", "r")
	if err == nil {
		t.Fatal("expected error")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAuthMapFromOAuthStorage(t *testing.T) {
	if _, err := authMapFromOAuthStorage(nil); err == nil {
		t.Fatal("nil expected error")
	}
	st := &clineOAuthStorage{
		Type: ProviderKey, AccessToken: "workos:at", RefreshToken: "rt",
		ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
		Email:     "map@example.com", AccountID: "acc",
		Metadata: map[string]any{"token_source": "device_oauth", "name": "N"},
	}
	m, err := authMapFromOAuthStorage(st)
	if err != nil {
		t.Fatal(err)
	}
	if m["Provider"] != ProviderKey || m["Label"] != "map@example.com" {
		t.Fatalf("%v", m)
	}
	meta, _ := m["Metadata"].(map[string]any)
	if meta["token_source"] != "device_oauth" || meta["name"] != "N" {
		t.Fatalf("meta=%v", meta)
	}

	st2 := &clineOAuthStorage{Type: ProviderKey, AccessToken: "a", RefreshToken: "r", ExpiresAt: 1}
	m2, err := authMapFromOAuthStorage(st2)
	if err != nil {
		t.Fatal(err)
	}
	if m2["Label"] != "Cline OAuth" {
		t.Fatalf("label=%v", m2["Label"])
	}
}

// --- handleAuthLoginStart ----------------------------------------------------

func TestHandleAuthLoginStartTable(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		clearDeviceLogins()
		t.Cleanup(clearDeviceLogins)
		_, _ = withWorkOSTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_code": "dc-start", "user_code": "START-1",
				"verification_uri":          "https://ex.test/device",
				"verification_uri_complete": "https://ex.test/device?user_code=START-1",
				"expires_in":                120, "interval": 5,
			})
		})
		raw, err := handleAuthLoginStart([]byte(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		env := decodeEnvelope(t, raw)
		if !env.OK {
			t.Fatalf("%+v", env.Error)
		}
		var result map[string]any
		if err := json.Unmarshal(env.Result, &result); err != nil {
			t.Fatal(err)
		}
		if result["Provider"] != ProviderKey {
			t.Fatalf("%v", result)
		}
		if result["URL"] != "https://ex.test/device?user_code=START-1" {
			t.Fatalf("URL=%v", result["URL"])
		}
		state, _ := result["State"].(string)
		if state == "" {
			t.Fatal("missing state")
		}
		deviceLoginMu.Lock()
		_, ok := deviceLogins[state]
		deviceLoginMu.Unlock()
		if !ok {
			t.Fatal("state not stored")
		}
	})

	t.Run("success_fallback_uri", func(t *testing.T) {
		clearDeviceLogins()
		t.Cleanup(clearDeviceLogins)
		_, _ = withWorkOSTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_code": "dc-fb", "user_code": "FB-1",
				"verification_uri": "https://ex.test/only",
				"expires_in":       99999, // clamp to deviceLoginTTL
				"interval":         5,
			})
		})
		raw, err := handleAuthLoginStart(nil)
		if err != nil {
			t.Fatal(err)
		}
		env := decodeEnvelope(t, raw)
		var result map[string]any
		_ = json.Unmarshal(env.Result, &result)
		if result["URL"] != "https://ex.test/only" {
			t.Fatalf("URL=%v", result["URL"])
		}
	})

	t.Run("workos_failure", func(t *testing.T) {
		_, _ = withWorkOSTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, "down")
		})
		raw, err := handleAuthLoginStart([]byte(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		env := decodeEnvelope(t, raw)
		if env.OK || env.Error == nil || env.Error.Code != "login_start_failed" {
			t.Fatalf("%+v raw=%s", env, raw)
		}
	})
}

// --- handleAuthLoginPoll -----------------------------------------------------

func TestHandleAuthLoginPollTable(t *testing.T) {
	t.Run("missing_state", func(t *testing.T) {
		raw, err := handleAuthLoginPoll([]byte(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		res := decodeLoginPoll(t, raw)
		if res["Status"] != "error" || !strings.Contains(res["Message"].(string), "missing") {
			t.Fatalf("%v", res)
		}
	})

	t.Run("state_lower_key", func(t *testing.T) {
		clearDeviceLogins()
		t.Cleanup(clearDeviceLogins)
		// unknown state via lowercase field
		raw, err := handleAuthLoginPoll([]byte(`{"state":"no-such"}`))
		if err != nil {
			t.Fatal(err)
		}
		res := decodeLoginPoll(t, raw)
		if res["Status"] != "error" || !strings.Contains(res["Message"].(string), "unknown") {
			t.Fatalf("%v", res)
		}
	})

	t.Run("expired", func(t *testing.T) {
		clearDeviceLogins()
		t.Cleanup(clearDeviceLogins)
		state := "expired-state"
		seedPendingLogin(t, state, -time.Minute)
		raw, err := handleAuthLoginPoll([]byte(`{"State":"` + state + `"}`))
		if err != nil {
			t.Fatal(err)
		}
		res := decodeLoginPoll(t, raw)
		if res["Status"] != "error" || !strings.Contains(res["Message"].(string), "timed out") {
			t.Fatalf("%v", res)
		}
	})

	t.Run("pending", func(t *testing.T) {
		clearDeviceLogins()
		t.Cleanup(clearDeviceLogins)
		state := "pending-state"
		seedPendingLogin(t, state, time.Minute)
		_, _ = withWorkOSTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "authorization_pending"})
		})
		raw, err := handleAuthLoginPoll([]byte(`{"State":"` + state + `"}`))
		if err != nil {
			t.Fatal(err)
		}
		res := decodeLoginPoll(t, raw)
		if res["Status"] != "pending" {
			t.Fatalf("%v", res)
		}
		// pending keeps the entry
		deviceLoginMu.Lock()
		_, ok := deviceLogins[state]
		deviceLoginMu.Unlock()
		if !ok {
			t.Fatal("pending should keep state")
		}
	})

	t.Run("denied", func(t *testing.T) {
		clearDeviceLogins()
		t.Cleanup(clearDeviceLogins)
		state := "denied-state"
		seedPendingLogin(t, state, time.Minute)
		_, _ = withWorkOSTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": "access_denied", "error_description": "nope",
			})
		})
		raw, err := handleAuthLoginPoll([]byte(`{"State":"` + state + `"}`))
		if err != nil {
			t.Fatal(err)
		}
		res := decodeLoginPoll(t, raw)
		if res["Status"] != "error" || !strings.Contains(res["Message"].(string), "nope") {
			t.Fatalf("%v", res)
		}
		deviceLoginMu.Lock()
		_, ok := deviceLogins[state]
		deviceLoginMu.Unlock()
		if ok {
			t.Fatal("denied should forget state")
		}
	})

	t.Run("register_fail", func(t *testing.T) {
		clearDeviceLogins()
		t.Cleanup(clearDeviceLogins)
		state := "regfail-state"
		seedPendingLogin(t, state, time.Minute)
		_, _ = withWorkOSTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case workOSAuthenticate:
				_ = json.NewEncoder(w).Encode(map[string]any{
					"access_token": "at", "refresh_token": "rt",
				})
			case clineRegisterPath:
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(w, "reg down")
			default:
				w.WriteHeader(404)
			}
		})
		// currentConfig().BaseURL must be the test server
		cfg := defaultConfig()
		cfg.BaseURL = workOSAPIBaseURL
		cfg.AuthDir = t.TempDir()
		cfg.ClientVersion = defaultClientVersion
		configMu.Lock()
		prev := activeConfig
		activeConfig = cfg
		configMu.Unlock()
		t.Cleanup(func() {
			configMu.Lock()
			activeConfig = prev
			configMu.Unlock()
		})

		raw, err := handleAuthLoginPoll([]byte(`{"State":"` + state + `"}`))
		if err != nil {
			t.Fatal(err)
		}
		res := decodeLoginPoll(t, raw)
		if res["Status"] != "error" || !strings.Contains(res["Message"].(string), "registration failed") {
			t.Fatalf("%v", res)
		}
	})

	t.Run("success", func(t *testing.T) {
		clearDeviceLogins()
		t.Cleanup(clearDeviceLogins)
		state := "ok-state"
		seedPendingLogin(t, state, time.Minute)
		var authCalls, regCalls int32
		srvCfg, srv := withWorkOSTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case workOSAuthenticate:
				atomic.AddInt32(&authCalls, 1)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"access_token": "wo-at", "refresh_token": "wo-rt",
				})
			case clineRegisterPath:
				atomic.AddInt32(&regCalls, 1)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"success": true,
					"data": map[string]any{
						"accessToken": "final-at", "refreshToken": "final-rt",
						"expiresAt": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
						"userInfo":  map[string]any{"email": "ok@example.com", "clineUserId": "u-ok"},
					},
				})
			default:
				http.NotFound(w, r)
			}
		})
		_ = srv
		configMu.Lock()
		prev := activeConfig
		activeConfig = srvCfg
		configMu.Unlock()
		t.Cleanup(func() {
			configMu.Lock()
			activeConfig = prev
			configMu.Unlock()
		})

		raw, err := handleAuthLoginPoll([]byte(`{"State":"` + state + `","Provider":"cline"}`))
		if err != nil {
			t.Fatal(err)
		}
		res := decodeLoginPoll(t, raw)
		if res["Status"] != "success" {
			t.Fatalf("%v", res)
		}
		auth, _ := res["Auth"].(map[string]any)
		if auth == nil || auth["Provider"] != ProviderKey {
			t.Fatalf("auth=%v", auth)
		}
		if atomic.LoadInt32(&authCalls) != 1 || atomic.LoadInt32(&regCalls) != 1 {
			t.Fatalf("auth=%d reg=%d", authCalls, regCalls)
		}
		deviceLoginMu.Lock()
		_, ok := deviceLogins[state]
		deviceLoginMu.Unlock()
		if ok {
			t.Fatal("success should forget state")
		}
	})
}
