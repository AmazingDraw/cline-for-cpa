package plugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestQuotaIdentifierDescribeReset(t *testing.T) {
	idRaw, err := handleQuotaIdentifier(nil)
	if err != nil {
		t.Fatal(err)
	}
	id := decodeEnvelope(t, idRaw)
	if !id.OK {
		t.Fatalf("identifier not ok: %s", idRaw)
	}
	var idResult map[string]string
	if err := json.Unmarshal(id.Result, &idResult); err != nil {
		t.Fatal(err)
	}
	if idResult["identifier"] != ProviderKey {
		t.Fatalf("identifier=%q want %q", idResult["identifier"], ProviderKey)
	}

	descRaw, err := handleQuotaDescribe(nil)
	if err != nil {
		t.Fatal(err)
	}
	desc := decodeEnvelope(t, descRaw)
	var descResult map[string]any
	if err := json.Unmarshal(desc.Result, &descResult); err != nil {
		t.Fatal(err)
	}
	if descResult["display_name"] != PluginTitle {
		t.Fatalf("display_name=%v", descResult["display_name"])
	}
	if descResult["supports_reset"] != false {
		t.Fatalf("supports_reset=%v want false", descResult["supports_reset"])
	}
	providers, _ := descResult["supported_providers"].([]any)
	if len(providers) != 1 || providers[0] != ProviderKey {
		t.Fatalf("supported_providers=%v", descResult["supported_providers"])
	}

	resetRaw, err := handleQuotaReset(nil)
	if err != nil {
		t.Fatal(err)
	}
	reset := decodeEnvelope(t, resetRaw)
	var resetResult map[string]any
	if err := json.Unmarshal(reset.Result, &resetResult); err != nil {
		t.Fatal(err)
	}
	if resetResult["success"] != false {
		t.Fatalf("reset success=%v", resetResult["success"])
	}
}

func TestFreeTierQuotaFetchShape(t *testing.T) {
	got := freeTierQuotaFetch()
	sub, _ := got["subscription"].(map[string]any)
	if sub["plan"] != "Free Tier" || sub["tierId"] != "free" {
		t.Fatalf("subscription=%v", sub)
	}
	groups, _ := got["groups"].([]map[string]any)
	if len(groups) != 1 {
		t.Fatalf("groups=%v", got["groups"])
	}
}

func TestPlanToQuotaFetchWindowsAndStatus(t *testing.T) {
	env := &clinePlanEnvelope{Success: true}
	env.Data.CanceledAt = "2026-01-15T00:00:00Z"
	env.Data.CurrentPeriodEnd = "2026-02-01T00:00:00Z"
	env.Data.Plan.ID = "pass"
	env.Data.Plan.Name = "Pass"
	env.Data.Plan.DisplayName = "Cline Pass"
	env.Data.Plan.Interval = "month"
	env.Data.Plan.PricePerSeatCents = 2000
	env.Data.Plan.IsActive = true

	limits := &clineUsageLimitsEnvelope{Success: true}
	limits.Data.Limits = []struct {
		Type        string  `json:"type"`
		PercentUsed float64 `json:"percentUsed"`
		ResetsAt    string  `json:"resetsAt"`
	}{
		{Type: "five_hour", PercentUsed: 25, ResetsAt: "t1"},
		{Type: "weekly", PercentUsed: -10, ResetsAt: "t2"},
		{Type: "monthly", PercentUsed: 150, ResetsAt: "t3"},
		{Type: "custom", PercentUsed: 10, ResetsAt: "t4"},
	}

	got := planToQuotaFetch(env, limits, nil)
	sub := got["subscription"].(map[string]any)
	if sub["plan"] != "Cline Pass" || sub["tierId"] != "pass" {
		t.Fatalf("subscription=%v", sub)
	}
	summary := got["summary"].([]map[string]any)
	if summary[0]["key"] != "price" || summary[0]["value"] != 20.0 {
		t.Fatalf("price summary=%v", summary[0])
	}
	if summary[1]["unit"] != "canceling" || summary[1]["value"] != 0.5 {
		t.Fatalf("canceling status=%v", summary[1])
	}
	groups := got["groups"].([]map[string]any)
	buckets := groups[0]["buckets"].([]map[string]any)
	if len(buckets) != 4 {
		t.Fatalf("want 4 buckets, got %d", len(buckets))
	}
	if buckets[0]["window"] != "5h" || buckets[0]["remainingFraction"] != 0.75 {
		t.Fatalf("5h bucket=%v", buckets[0])
	}
	if buckets[1]["window"] != "7d" || buckets[1]["remainingFraction"] != 1.0 {
		t.Fatalf("clamped unused weekly=%v", buckets[1])
	}
	if buckets[2]["window"] != "period" || buckets[2]["remainingFraction"] != 0.0 {
		t.Fatalf("clamped monthly=%v", buckets[2])
	}
	if buckets[3]["window"] != "custom" {
		t.Fatalf("passthrough window=%v", buckets[3])
	}

	inactive := *env
	inactive.Data.Plan.IsActive = false
	inactive.Data.Plan.DisplayName = ""
	inactive.Data.Plan.Name = "Legacy"
	inactive.Data.Plan.PricePerSeatCents = 0
	fallback := planToQuotaFetch(&inactive, nil, errors.New("usage-limits HTTP 500"))
	sum := fallback["summary"].([]map[string]any)
	if sum[0]["unit"] != "inactive" || sum[0]["value"] != 0.0 {
		t.Fatalf("inactive status=%v", sum[0])
	}
	if sum[1]["key"] != "usage_limits_error" {
		t.Fatalf("want usage_limits_error, got %v", sum)
	}
	fbGroups := fallback["groups"].([]map[string]any)
	fbBuckets := fbGroups[0]["buckets"].([]map[string]any)
	if len(fbBuckets) != 1 || fbBuckets[0]["window"] != "period" {
		t.Fatalf("fallback buckets=%v", fbBuckets)
	}
	if fbGroups[0]["displayName"] != "Legacy" {
		t.Fatalf("display fallback=%v", fbGroups[0]["displayName"])
	}
}

func TestFetchClinePlanAndUsageLimits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			http.Error(w, "missing bearer", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/users/me/plan":
			_, _ = w.Write([]byte(`{"success":true,"data":{"plan":{"id":"p1","name":"Pass","displayName":"Cline Pass","interval":"month","pricePerSeatCents":800,"isActive":true}}}`))
		case "/users/me/plan/usage-limits":
			_, _ = w.Write([]byte(`{"success":true,"data":{"limits":[{"type":"5h","percentUsed":40,"resetsAt":"soon"}]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cfg := pluginConfig{BaseURL: srv.URL, ClientVersion: "0.0.34"}
	plan, err := fetchClinePlan(cfg, "tok")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.Data.Plan.ID != "p1" {
		t.Fatalf("plan id=%q", plan.Data.Plan.ID)
	}
	limits, err := fetchClineUsageLimits(cfg, "tok")
	if err != nil {
		t.Fatalf("limits: %v", err)
	}
	if len(limits.Data.Limits) != 1 || limits.Data.Limits[0].Type != "5h" {
		t.Fatalf("limits=%+v", limits.Data.Limits)
	}
}

func TestFetchClinePlanErrorPaths(t *testing.T) {
	t.Run("http 500", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		}))
		defer srv.Close()
		_, err := fetchClinePlan(pluginConfig{BaseURL: srv.URL}, "tok")
		if err == nil || !strings.Contains(err.Error(), "plan HTTP 500") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("invalid json", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("not-json"))
		}))
		defer srv.Close()
		_, err := fetchClinePlan(pluginConfig{BaseURL: srv.URL}, "tok")
		if err == nil {
			t.Fatal("expected unmarshal error")
		}
	})
	t.Run("unsuccessful", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"success":false}`))
		}))
		defer srv.Close()
		_, err := fetchClinePlan(pluginConfig{BaseURL: srv.URL}, "tok")
		if err == nil || !strings.Contains(err.Error(), "unsuccessful") {
			t.Fatalf("err=%v", err)
		}
		_, err = fetchClineUsageLimits(pluginConfig{BaseURL: srv.URL}, "tok")
		if err == nil || !strings.Contains(err.Error(), "unsuccessful") {
			t.Fatalf("limits err=%v", err)
		}
	})
}

func TestHandleQuotaFetchNoCredentials(t *testing.T) {
	// The env channel was removed (keys now flow through the api_keys array
	// only), and the config key is no longer a quota fallback — an empty
	// request with no credential must fail instead of borrowing one.
	applyTestConfig(t, fmt.Sprintf("auth_dir: %q\napi_key: \"\"\n", t.TempDir()))
	raw, err := handleQuotaFetch([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	env := decodeEnvelope(t, raw)
	if env.OK || env.Error == nil || env.Error.Code != "quota_fetch_failed" {
		t.Fatalf("want quota_fetch_failed, got %s", raw)
	}
	if !strings.Contains(env.Error.Message, "no credentials") {
		t.Fatalf("message=%q", env.Error.Message)
	}
}

func TestHandleQuotaFetchAPIKeyHappyPath(t *testing.T) {
	srv := newQuotaTestServer(t, http.StatusOK, true)
	defer srv.Close()
	// The key must arrive the way the host delivers it: as the credential's
	// own storage, not borrowed from the config. The config deliberately keeps
	// no key — if the code still had a config fallback, the next test would
	// catch it; this one just has to prove the storage path works.
	applyTestConfig(t, fmt.Sprintf("auth_dir: %q\nbase_url: %q\n", t.TempDir(), srv.URL))

	storage, _ := json.Marshal(clineOAuthStorage{Type: ProviderKey, APIKey: "sk_cfg"})
	req, _ := json.Marshal(map[string]any{"storage_json": storage})
	raw, err := handleQuotaFetch(req)
	if err != nil {
		t.Fatal(err)
	}
	env := decodeEnvelope(t, raw)
	if !env.OK {
		t.Fatalf("expected ok: %s", raw)
	}
	var result map[string]any
	if err := json.Unmarshal(env.Result, &result); err != nil {
		t.Fatal(err)
	}
	sub := result["subscription"].(map[string]any)
	if sub["tierId"] != "p1" {
		t.Fatalf("subscription=%v", sub)
	}
}

// A config key must never be borrowed for a credential-less quota request:
// the card the operator switched off would otherwise keep reporting quota
// sourced from the very key the toggle had just switched off.
func TestHandleQuotaFetchNeverUsesConfigAPIKey(t *testing.T) {
	srv := newQuotaTestServer(t, http.StatusOK, true)
	defer srv.Close()
	applyTestConfig(t, fmt.Sprintf("auth_dir: %q\nbase_url: %q\napi_key: sk_config_must_not_be_used\n", t.TempDir(), srv.URL))

	raw, err := handleQuotaFetch([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	env := decodeEnvelope(t, raw)
	if env.OK || env.Error == nil || env.Error.Code != "quota_fetch_failed" {
		t.Fatalf("want quota_fetch_failed, got %s", raw)
	}
	if !strings.Contains(env.Error.Message, "no credentials") {
		t.Fatalf("message=%q", env.Error.Message)
	}
}

func TestHandleQuotaFetchStorageJSONAndCap(t *testing.T) {
	srv := newQuotaTestServer(t, http.StatusOK, true)
	defer srv.Close()
	applyTestConfig(t, fmt.Sprintf("auth_dir: %q\nbase_url: %q\n", t.TempDir(), srv.URL))

	storage, _ := json.Marshal(clineOAuthStorage{Type: ProviderKey, APIKey: "sk_stored"})
	req, _ := json.Marshal(map[string]any{"storage_json": storage})
	raw, err := handleQuotaFetch(req)
	if err != nil {
		t.Fatal(err)
	}
	if env := decodeEnvelope(t, raw); !env.OK {
		t.Fatalf("storage_json: %s", raw)
	}

	reqCap, _ := json.Marshal(map[string]any{"StorageJSON": storage})
	raw, err = handleQuotaFetch(reqCap)
	if err != nil {
		t.Fatal(err)
	}
	if env := decodeEnvelope(t, raw); !env.OK {
		t.Fatalf("StorageJSON: %s", raw)
	}
}

func TestHandleQuotaFetchFreshOAuth(t *testing.T) {
	srv := newQuotaTestServer(t, http.StatusOK, false)
	defer srv.Close()
	applyTestConfig(t, fmt.Sprintf("auth_dir: %q\nbase_url: %q\n", t.TempDir(), srv.URL))

	storage, _ := json.Marshal(clineOAuthStorage{
		Type:         ProviderKey,
		AccessToken:  "live-access",
		RefreshToken: "live-refresh",
		ExpiresAt:    time.Now().Add(2 * time.Hour).UnixMilli(),
	})
	req, _ := json.Marshal(map[string]any{"storage_json": storage})
	raw, err := handleQuotaFetch(req)
	if err != nil {
		t.Fatal(err)
	}
	if env := decodeEnvelope(t, raw); !env.OK {
		t.Fatalf("oauth fetch: %s", raw)
	}
}

func TestHandleQuotaFetchPlan404IsFreeTier(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"no plan"}`))
	}))
	defer srv.Close()
	applyTestConfig(t, fmt.Sprintf("auth_dir: %q\nbase_url: %q\n", t.TempDir(), srv.URL))

	storage, _ := json.Marshal(clineOAuthStorage{Type: ProviderKey, APIKey: "sk_free"})
	req, _ := json.Marshal(map[string]any{"storage_json": storage})
	raw, err := handleQuotaFetch(req)
	if err != nil {
		t.Fatal(err)
	}
	env := decodeEnvelope(t, raw)
	if !env.OK {
		t.Fatalf("404 should synthesize free tier, got %s", raw)
	}
	var result map[string]any
	if err := json.Unmarshal(env.Result, &result); err != nil {
		t.Fatal(err)
	}
	sub := result["subscription"].(map[string]any)
	if sub["tierId"] != "free" {
		t.Fatalf("subscription=%v", sub)
	}
}

func TestHandleQuotaFetchPlanError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("nope"))
	}))
	defer srv.Close()
	applyTestConfig(t, fmt.Sprintf("auth_dir: %q\nbase_url: %q\n", t.TempDir(), srv.URL))

	// The credential comes from storage so this test exercises what it always
	// meant to: the upstream 500 on the plan endpoint, not the missing-key
	// path it accidentally drifted into when the config fallback was removed.
	storage, _ := json.Marshal(clineOAuthStorage{Type: ProviderKey, APIKey: "sk_x"})
	req, _ := json.Marshal(map[string]any{"storage_json": storage})
	raw, err := handleQuotaFetch(req)
	if err != nil {
		t.Fatal(err)
	}
	env := decodeEnvelope(t, raw)
	if env.OK || env.Error == nil || env.Error.Code != "quota_fetch_failed" {
		t.Fatalf("want quota_fetch_failed, got %s", raw)
	}
}

func TestHandleQuotaFetchReauthRequired(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == refreshPath {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"expired"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	applyTestConfig(t, fmt.Sprintf("auth_dir: %q\nbase_url: %q\n", t.TempDir(), srv.URL))

	storage, _ := json.Marshal(clineOAuthStorage{
		Type:         ProviderKey,
		AccessToken:  "stale",
		RefreshToken: "dead-refresh",
		ExpiresAt:    time.Now().Add(-time.Minute).UnixMilli(),
	})
	req, _ := json.Marshal(map[string]any{"storage_json": storage})
	raw, err := handleQuotaFetch(req)
	if err != nil {
		t.Fatal(err)
	}
	env := decodeEnvelope(t, raw)
	if env.OK || env.Error == nil || env.Error.Code != "cline_reauth_required" {
		t.Fatalf("want cline_reauth_required, got %s", raw)
	}
}

func TestHandleQuotaFetchOAuthRefreshTransient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == refreshPath {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"temporarily_unavailable"}`))
			return
		}
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()
	applyTestConfig(t, fmt.Sprintf("auth_dir: %q\nbase_url: %q\n", t.TempDir(), srv.URL))

	storage, _ := json.Marshal(clineOAuthStorage{
		Type:         ProviderKey,
		AccessToken:  "stale",
		RefreshToken: "live-refresh",
		ExpiresAt:    time.Now().Add(-time.Minute).UnixMilli(),
	})
	req, _ := json.Marshal(map[string]any{"storage_json": storage})
	raw, err := handleQuotaFetch(req)
	if err != nil {
		t.Fatal(err)
	}
	env := decodeEnvelope(t, raw)
	if env.OK || env.Error == nil || env.Error.Code != "quota_fetch_failed" {
		t.Fatalf("want quota_fetch_failed, got %s", raw)
	}
}

func TestHandleQuotaFetchViaDispatcher(t *testing.T) {
	raw, err := HandleMethod("quota.identifier", nil)
	if err != nil {
		t.Fatal(err)
	}
	if env := decodeEnvelope(t, raw); !env.OK {
		t.Fatalf("quota.identifier: %s", raw)
	}
}

func newQuotaTestServer(t *testing.T, planStatus int, withLimits bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/users/me/plan":
			if planStatus != http.StatusOK {
				w.WriteHeader(planStatus)
				_, _ = w.Write([]byte(`{"error":"plan"}`))
				return
			}
			_, _ = w.Write([]byte(`{"success":true,"data":{"currentPeriodEnd":"2026-02-01T00:00:00Z","plan":{"id":"p1","name":"Pass","displayName":"Cline Pass","interval":"month","pricePerSeatCents":800,"isActive":true}}}`))
		case "/users/me/plan/usage-limits":
			if !withLimits {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"success":false}`))
				return
			}
			_, _ = w.Write([]byte(`{"success":true,"data":{"limits":[{"type":"weekly","percentUsed":10,"resetsAt":"soon"}]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
}
