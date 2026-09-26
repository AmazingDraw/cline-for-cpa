package plugin

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

func handleQuotaIdentifier(_ []byte) ([]byte, error) {
	return okEnvelope(map[string]string{"identifier": ProviderKey})
}

func handleQuotaDescribe(_ []byte) ([]byte, error) {
	return okEnvelope(map[string]any{
		"supported_providers": []string{ProviderKey},
		"display_name":        PluginTitle,
		"supports_reset":      false,
	})
}

func handleQuotaReset(_ []byte) ([]byte, error) {
	return okEnvelope(map[string]any{
		"success": false,
		"message": "Cline Pass quota cannot be reset from the plugin",
	})
}

func handleQuotaFetch(request []byte) ([]byte, error) {
	var req struct {
		StorageJSON    []byte         `json:"storage_json"`
		StorageJSONCap []byte         `json:"StorageJSON"`
		Metadata       map[string]any `json:"metadata"`
		AuthMetadata   map[string]any `json:"auth_metadata"`
		Provider       string         `json:"provider"`
	}
	_ = json.Unmarshal(request, &req)
	raw := req.StorageJSON
	if len(raw) == 0 {
		raw = req.StorageJSONCap
	}
	st := parseStorageFromRequest(raw, req.Metadata, req.AuthMetadata)
	cfg := currentConfig()
	bearer := ""
	if st != nil {
		if strings.TrimSpace(st.RefreshToken) != "" || strings.TrimSpace(st.AccessToken) != "" {
			fresh, err := ensureFreshOAuth(cfg, st)
			if err != nil {
				if isReauthRequired(err) {
					return ErrorEnvelope("cline_reauth_required",
						"Cline 登录已失效（refresh token 被拒），请重新授权 Cline。"), nil
				}
				return ErrorEnvelope("quota_fetch_failed", err.Error()), nil
			}
			bearer = oauthBearer(fresh)
		} else if k := strings.TrimSpace(st.APIKey); k != "" {
			bearer = k
		}
	}
	if bearer == "" {
		if k := resolveAPIKey(cfg); k != "" {
			bearer = k
		}
	}
	if bearer == "" {
		return ErrorEnvelope("quota_fetch_failed", "no credentials for quota fetch"), nil
	}

	plan, err := fetchClinePlan(cfg, bearer)
	if err != nil {
		if strings.Contains(err.Error(), "HTTP 404") {
			// Unsubscribed free-tier user: synthesize honest Free Tier quota response instead of failing
			return okEnvelope(freeTierQuotaFetch())
		}
		return ErrorEnvelope("quota_fetch_failed", err.Error()), nil
	}
	limits, limErr := fetchClineUsageLimits(cfg, bearer)
	// Usage-limits is preferred for meter bars; plan alone is enough for subscription.
	return okEnvelope(planToQuotaFetch(plan, limits, limErr))
}

func freeTierQuotaFetch() map[string]any {
	sub := map[string]any{
		"plan":     "Free Tier",
		"tierName": "Free Tier",
		"tierId":   "free",
	}
	summary := []map[string]any{
		{
			"key":      "price",
			"label":    "Plan price",
			"value":    0.0,
			"format":   "currency",
			"currency": "USD",
		},
		{
			"key":   "status",
			"label": "Subscription",
			"value": 1.0,
			"unit":  "free",
		},
	}
	groups := []map[string]any{
		{
			"displayName": "Cline Free Tier",
			"buckets": []map[string]any{
				{
					"window":            "period",
					"remainingFraction": 1.0,
					"description":       "Daily rate limits per model (sliding window)",
				},
			},
		},
	}
	return map[string]any{
		"subscription": sub,
		"summary":      summary,
		"groups":       groups,
	}
}

type clinePlanEnvelope struct {
	Success bool `json:"success"`
	Data    struct {
		PlanHistoryID      string `json:"planHistoryId"`
		UserID             string `json:"userId"`
		SubscriptionID     string `json:"subscriptionId"`
		CurrentPeriodStart string `json:"currentPeriodStart"`
		CurrentPeriodEnd   string `json:"currentPeriodEnd"`
		CancelAt           string `json:"cancelAt"`
		CanceledAt         string `json:"canceledAt"`
		Plan               struct {
			ID                string `json:"id"`
			Name              string `json:"name"`
			DisplayName       string `json:"displayName"`
			Description       string `json:"description"`
			Interval          string `json:"interval"`
			PricePerSeatCents int    `json:"pricePerSeatCents"`
			IsActive          bool   `json:"isActive"`
			Entitlements      map[string]struct {
				Enabled               bool `json:"enabled"`
				InferenceCapThreshold struct {
					Last5HoursUsageCostUSDPerUser float64 `json:"last5HoursUsageCostUSDPerUser"`
					Last7daysUsageCostUSDPerUser  float64 `json:"last7daysUsageCostUSDPerUser"`
					Last30daysUsageCostUSDPerUser float64 `json:"last30daysUsageCostUSDPerUser"`
				} `json:"inferenceCapThreshold"`
			} `json:"entitlements"`
		} `json:"plan"`
	} `json:"data"`
}

func fetchClinePlan(cfg pluginConfig, bearer string) (*clinePlanEnvelope, error) {
	base := strings.TrimRight(cfg.BaseURL, "/")
	if base == "" {
		base = defaultBaseURL
	}
	req, err := http.NewRequest(http.MethodGet, base+"/users/me/plan", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	applyClineHeaders(req, cfg)
	req.Header.Set("Accept", "application/json")
	resp, err := upstreamClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("plan HTTP %d: %s", resp.StatusCode, truncate(raw, 256))
	}
	var env clinePlanEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, err
	}
	if !env.Success {
		return nil, fmt.Errorf("plan response unsuccessful")
	}
	return &env, nil
}

type clineUsageLimitsEnvelope struct {
	Success bool `json:"success"`
	Data    struct {
		Limits []struct {
			Type        string  `json:"type"`
			PercentUsed float64 `json:"percentUsed"`
			ResetsAt    string  `json:"resetsAt"`
		} `json:"limits"`
	} `json:"data"`
}

func fetchClineUsageLimits(cfg pluginConfig, bearer string) (*clineUsageLimitsEnvelope, error) {
	base := strings.TrimRight(cfg.BaseURL, "/")
	if base == "" {
		base = defaultBaseURL
	}
	req, err := http.NewRequest(http.MethodGet, base+"/users/me/plan/usage-limits", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	applyClineHeaders(req, cfg)
	req.Header.Set("Accept", "application/json")
	resp, err := upstreamClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("usage-limits HTTP %d: %s", resp.StatusCode, truncate(raw, 256))
	}
	var env clineUsageLimitsEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, err
	}
	if !env.Success {
		return nil, fmt.Errorf("usage-limits response unsuccessful")
	}
	return &env, nil
}

func planToQuotaFetch(env *clinePlanEnvelope, limits *clineUsageLimitsEnvelope, limErr error) map[string]any {
	p := env.Data.Plan
	display := strings.TrimSpace(p.DisplayName)
	if display == "" {
		display = strings.TrimSpace(p.Name)
	}
	sub := map[string]any{
		"plan":     display,
		"tierName": firstNonEmpty(p.Interval, "Cline Pass"),
		"tierId":   p.ID,
	}

	summary := []map[string]any{}
	if p.PricePerSeatCents > 0 {
		summary = append(summary, map[string]any{
			"key":      "price",
			"label":    "Plan price",
			"value":    float64(p.PricePerSeatCents) / 100.0,
			"format":   "currency",
			"currency": "USD",
		})
	}
	// Match Cline UI: plan.IsActive stays "active" through cancel-at-period-end.
	status := "active"
	if !p.IsActive {
		status = "inactive"
	} else if strings.TrimSpace(env.Data.CanceledAt) != "" {
		status = "canceling"
	}
	statusValue := map[string]float64{"active": 1, "canceling": 0.5, "inactive": 0}[status]
	summary = append(summary, map[string]any{
		"key":   "status",
		"label": "Subscription",
		"value": statusValue,
		"unit":  status,
	})

	buckets := []map[string]any{}
	if limits != nil && limErr == nil {
		for _, lim := range limits.Data.Limits {
			typ := strings.ToLower(strings.TrimSpace(lim.Type))
			window := typ
			desc := typ
			switch typ {
			case "five_hour", "5h":
				window, desc = "5h", "5-Hour Limit"
			case "weekly", "7d":
				window, desc = "7d", "Weekly Limit"
			case "monthly", "30d", "period":
				window, desc = "period", "Monthly Limit"
			}
			used := lim.PercentUsed
			if used < 0 {
				used = 0
			}
			if used > 100 {
				used = 100
			}
			remaining := (100.0 - used) / 100.0
			reset := strings.TrimSpace(lim.ResetsAt)
			buckets = append(buckets, map[string]any{
				"window":            window,
				"remainingFraction": remaining,
				"resetTime":         reset,
				"description":       fmt.Sprintf("%s — %.0f%% used", desc, used),
			})
		}
	}
	if len(buckets) == 0 {
		// Fallback: plan-only (no usage-limits). Prefer honest empty over fake bars.
		reset := strings.TrimSpace(env.Data.CurrentPeriodEnd)
		if limErr != nil {
			summary = append(summary, map[string]any{
				"key":   "usage_limits_error",
				"label": "Usage limits",
				"value": 0,
				"unit":  limErr.Error(),
			})
		}
		if reset != "" {
			buckets = append(buckets, map[string]any{
				"window":            "period",
				"remainingFraction": 1.0,
				"resetTime":         reset,
				"description":       "Billing period (usage-limits unavailable)",
			})
		}
	}

	groups := []map[string]any{
		{
			"displayName": display,
			"buckets":     buckets,
		},
	}
	return map[string]any{
		"subscription": sub,
		"summary":      summary,
		"groups":       groups,
	}
}
