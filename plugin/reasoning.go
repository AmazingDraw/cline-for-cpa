package plugin

import (
	"fmt"
	"strings"
	"sync"
)

// reasoning_effort handling.
//
// The upstream is case-sensitive and returns an EMPTY response (silent failure,
// the same "装死" signature seen elsewhere) for any value outside its whitelist —
// verified 2026-09-23 with 11 A/B calls against cline-pass/deepseek-v4.1-flash:
//
//	absent / none / minimal / low / medium / high / xhigh  → ok
//	"bogus" / "" / "HIGH"                                  → empty response
//
// A client that sends a dirty value therefore gets a silent dead turn. We repair
// the harmless variants (whitespace, letter case) and drop anything else, which
// falls back to the upstream default — exactly what a well-behaved client does.
var allowedReasoningEfforts = map[string]bool{
	"none":    true,
	"minimal": true,
	"low":     true,
	"medium":  true,
	"high":    true,
	"xhigh":   true,
}

var (
	reasoningDropOnce   = map[string]bool{}
	reasoningDropOnceMu sync.Mutex
)

func reasoningEffortNormalizeEnabled(cfg pluginConfig) bool {
	return cfg.ReasoningEffortNormalize == nil || *cfg.ReasoningEffortNormalize
}

// normalizeReasoningEffortInPayload repairs or removes the field in place.
// Returns (action, value) for logging: action is "repaired", "dropped" or "".
func normalizeReasoningEffortInPayload(obj map[string]any, cfg pluginConfig) (string, string) {
	if obj == nil || !reasoningEffortNormalizeEnabled(cfg) {
		return "", ""
	}
	raw, present := obj["reasoning_effort"]
	if !present {
		return "", ""
	}
	value, ok := raw.(string)
	if !ok {
		delete(obj, "reasoning_effort")
		return "dropped", fmt.Sprintf("%v", raw)
	}
	normalized := strings.ToLower(strings.TrimSpace(value))
	if allowedReasoningEfforts[normalized] {
		if normalized != value {
			obj["reasoning_effort"] = normalized
			return "repaired", fmt.Sprintf("%q → %q", value, normalized)
		}
		return "", ""
	}
	delete(obj, "reasoning_effort")
	return "dropped", fmt.Sprintf("%q", value)
}

// logReasoningEffortEvent reports a repair/drop once per distinct value per
// process, so a noisy client cannot flood the log.
func logReasoningEffortEvent(cfg pluginConfig, action, detail string) {
	if action == "" {
		return
	}
	key := action + ":" + detail
	reasoningDropOnceMu.Lock()
	seen := reasoningDropOnce[key]
	reasoningDropOnce[key] = true
	reasoningDropOnceMu.Unlock()
	if seen {
		return
	}
	logCredentialEvent(cfg, "reasoning_effort %s: %s (upstream rejects unknown values with an empty response)", action, detail)
}
