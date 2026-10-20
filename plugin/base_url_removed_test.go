package plugin

import (
	"testing"
)

// withUpstreamBaseURL points every upstream call at base (typically an
// httptest server) for the duration of the test. Empty restores the default.
func withUpstreamBaseURL(t *testing.T, base string) {
	t.Helper()
	upstreamBaseMu.Lock()
	prev := upstreamBaseURL
	if base == "" {
		base = defaultBaseURL
	}
	upstreamBaseURL = base
	upstreamBaseMu.Unlock()
	t.Cleanup(func() {
		upstreamBaseMu.Lock()
		upstreamBaseURL = prev
		upstreamBaseMu.Unlock()
	})
}

// TestLegacyBaseURLConfigIgnored: an old host config that still carries
// base_url must load without error and must not move the upstream.
func TestLegacyBaseURLConfigIgnored(t *testing.T) {
	for _, raw := range []string{
		"base_url: http://127.0.0.1:9/evil\nfirst_frame_timeout_seconds: 42\n",
		`{"base_url": "http://127.0.0.1:9/evil", "first_frame_timeout_seconds": 42}`,
	} {
		if err := applyConfig([]byte(raw)); err != nil {
			t.Fatalf("legacy config %q must load: %v", raw, err)
		}
		if got := clineBaseURL(); got != defaultBaseURL {
			t.Fatalf("base_url must be ignored, upstream = %q", got)
		}
		if got := currentConfig().FirstFrameTimeoutSeconds; got != 42 {
			t.Fatalf("other fields must still apply, first_frame = %d", got)
		}
	}
	t.Cleanup(func() { _ = applyConfig(nil) })
}
