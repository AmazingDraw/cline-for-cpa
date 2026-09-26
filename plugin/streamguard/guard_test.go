package streamguard

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestFirstFrameTimeout(t *testing.T) {
	clock := NewManualClock(time.Unix(0, 0))
	var got atomic.Pointer[StallError]
	g := New(Config{
		FirstFrame:    5 * time.Second,
		Silence:       10 * time.Second,
		HeartbeatOnly: 20 * time.Second,
	}, clock, func(e *StallError) { got.Store(e) })
	g.Start()
	if clock.Armed() != 1 {
		t.Fatalf("expected first-frame timer armed, got %d", clock.Armed())
	}
	clock.AdvanceTo(time.Unix(4, 0))
	if got.Load() != nil {
		t.Fatalf("premature stall: %v", got.Load())
	}
	clock.AdvanceTo(time.Unix(5, 0))
	err := got.Load()
	if err == nil {
		t.Fatal("expected first-frame stall")
	}
	if err.Kind != KindFirstFrame {
		t.Fatalf("kind=%s want %s", err.Kind, KindFirstFrame)
	}
	if clock.Armed() != 0 {
		t.Fatalf("timer should be clear after fire, armed=%d", clock.Armed())
	}
}

func TestSilenceAfterFirstFrame(t *testing.T) {
	clock := NewManualClock(time.Unix(0, 0))
	var got atomic.Pointer[StallError]
	silence := 3 * time.Second
	g := New(Config{
		FirstFrame:    60 * time.Second,
		Silence:       silence,
		HeartbeatOnly: 30 * time.Second,
	}, clock, func(e *StallError) { got.Store(e) })
	g.Start()
	g.NoteInbound(false) // first meaningful frame
	if !g.State().FirstSeen {
		t.Fatal("first frame not recorded")
	}
	// Silence deadline is the earlier of the two → fires at t=3s.
	clock.AdvanceTo(time.Unix(2, 0))
	if got.Load() != nil {
		t.Fatalf("premature: %v", got.Load())
	}
	clock.AdvanceTo(time.Unix(3, 0))
	err := got.Load()
	if err == nil {
		t.Fatal("expected silence stall")
	}
	if err.Kind != KindSilence {
		t.Fatalf("kind=%s want %s msg=%q", err.Kind, KindSilence, err.Message)
	}
}

func TestSilenceDeadlineWinsMin(t *testing.T) {
	// OpenCodex load-bearing case: dropping min() would relax silence detection
	// to the heartbeat-only budget while still producing a "stall" message.
	clock := NewManualClock(time.Unix(0, 0))
	var got atomic.Pointer[StallError]
	silence := 1 * time.Second
	heartbeatOnly := 10 * time.Second
	g := New(Config{
		FirstFrame:    60 * time.Second,
		Silence:       silence,
		HeartbeatOnly: heartbeatOnly,
	}, clock, func(e *StallError) { got.Store(e) })
	g.Start()
	g.NoteInbound(false)
	armedBefore := clock.Armed()
	if armedBefore != 1 {
		t.Fatalf("post-first timer not armed: %d", armedBefore)
	}
	clock.AdvanceTo(time.Unix(0, int64(silence-time.Millisecond)))
	if got.Load() != nil {
		t.Fatal("should not fire before silence deadline")
	}
	clock.AdvanceTo(time.Unix(0, int64(silence)))
	err := got.Load()
	if err == nil {
		t.Fatal("expected silence stall at S, not heartbeat-only at 10S")
	}
	if err.Kind != KindSilence {
		t.Fatalf("kind=%s — silence deadline must win the min()", err.Kind)
	}
	if clock.Armed() != 0 {
		t.Fatalf("nothing should remain armed after fire, armed=%d", clock.Armed())
	}
}

func TestHeartbeatOnlyStall(t *testing.T) {
	clock := NewManualClock(time.Unix(0, 0))
	var got atomic.Pointer[StallError]
	silence := 5 * time.Second
	heartbeatOnly := 4 * time.Second
	g := New(Config{
		FirstFrame:    60 * time.Second,
		Silence:       silence,
		HeartbeatOnly: heartbeatOnly,
	}, clock, func(e *StallError) { got.Store(e) })
	g.Start()
	g.NoteInbound(false) // t=0 meaningful

	// Keep refreshing silence clock with liveness-only frames, never meaningful.
	for tSec := 1; tSec <= 3; tSec++ {
		clock.AdvanceTo(time.Unix(int64(tSec), 0))
		g.NoteInbound(true) // liveness only
		if got.Load() != nil {
			t.Fatalf("premature stall at t=%d: %v", tSec, got.Load())
		}
	}
	// lastMeaningful still at t=0; heartbeat-only deadline = 4s.
	// lastInbound at t=3; silence deadline = 8s.
	// min → fires at t=4 as heartbeat-only.
	clock.AdvanceTo(time.Unix(4, 0))
	err := got.Load()
	if err == nil {
		t.Fatal("expected heartbeat-only stall")
	}
	if err.Kind != KindHeartbeatOnly {
		t.Fatalf("kind=%s want %s msg=%q", err.Kind, KindHeartbeatOnly, err.Message)
	}
}

func TestPluginKeepaliveDoesNotResetWatchdog(t *testing.T) {
	// Plugin/host keepalive must NEVER call NoteInbound — simulating that by
	// advancing time with no NoteInbound while "sending" keepalives.
	clock := NewManualClock(time.Unix(0, 0))
	var got atomic.Pointer[StallError]
	silence := 2 * time.Second
	g := New(Config{
		FirstFrame:    60 * time.Second,
		Silence:       silence,
		HeartbeatOnly: 30 * time.Second,
	}, clock, func(e *StallError) { got.Store(e) })
	g.Start()
	g.NoteInbound(false)

	// Simulate plugin keepalive every 500ms for 2s — WITHOUT NoteInbound.
	for _, at := range []time.Duration{
		500 * time.Millisecond,
		1000 * time.Millisecond,
		1500 * time.Millisecond,
		2000 * time.Millisecond,
	} {
		clock.AdvanceTo(time.Unix(0, int64(at)))
		// intentionally no NoteInbound — plugin keepalive ignored
	}
	err := got.Load()
	if err == nil {
		t.Fatal("plugin keepalive must not mask silence; expected silence stall")
	}
	if err.Kind != KindSilence {
		t.Fatalf("kind=%s want %s", err.Kind, KindSilence)
	}
}

func TestUpstreamLivenessResetsSilenceButNotMeaningful(t *testing.T) {
	clock := NewManualClock(time.Unix(0, 0))
	g := New(Config{
		FirstFrame:    60 * time.Second,
		Silence:       10 * time.Second,
		HeartbeatOnly: 20 * time.Second,
	}, clock, func(*StallError) {})
	g.Start()
	g.NoteInbound(false)
	first := g.State()
	clock.AdvanceTo(time.Unix(3, 0))
	g.NoteInbound(true) // upstream ping
	after := g.State()
	if !after.LastInbound.After(first.LastInbound) {
		t.Fatal("liveness must refresh lastInbound")
	}
	if !after.LastMeaningful.Equal(first.LastMeaningful) {
		t.Fatal("liveness must NOT refresh lastMeaningful")
	}
}

func TestMeaningfulResetsBoth(t *testing.T) {
	clock := NewManualClock(time.Unix(0, 0))
	g := New(Config{
		FirstFrame:    60 * time.Second,
		Silence:       10 * time.Second,
		HeartbeatOnly: 20 * time.Second,
	}, clock, func(*StallError) {})
	g.Start()
	g.NoteInbound(false)
	clock.AdvanceTo(time.Unix(2, 0))
	g.NoteInbound(true)
	mid := g.State()
	clock.AdvanceTo(time.Unix(4, 0))
	g.NoteInbound(false)
	end := g.State()
	if !end.LastInbound.After(mid.LastInbound) {
		t.Fatal("meaningful must refresh lastInbound")
	}
	if !end.LastMeaningful.After(mid.LastMeaningful) {
		t.Fatal("meaningful must refresh lastMeaningful")
	}
}

func TestDisarmOnCleanEnd(t *testing.T) {
	clock := NewManualClock(time.Unix(0, 0))
	var got atomic.Pointer[StallError]
	g := New(Config{
		FirstFrame:    5 * time.Second,
		Silence:       3 * time.Second,
		HeartbeatOnly: 10 * time.Second,
	}, clock, func(e *StallError) { got.Store(e) })
	g.Start()
	g.NoteInbound(false)
	g.Disarm()
	clock.AdvanceTo(time.Unix(30, 0))
	if got.Load() != nil {
		t.Fatalf("disarmed guard must not fire: %v", got.Load())
	}
	if clock.Armed() != 0 {
		t.Fatalf("disarm must clear timers, armed=%d", clock.Armed())
	}
}

func TestDefaultsMatchPlan(t *testing.T) {
	if DefaultFirstFrame != 60*time.Second {
		t.Fatalf("FirstFrame default=%s want 60s", DefaultFirstFrame)
	}
	if DefaultSilence != 60*time.Second {
		t.Fatalf("Silence default=%s want 60s", DefaultSilence)
	}
	if DefaultHeartbeatOnly != 180*time.Second {
		t.Fatalf("HeartbeatOnly default=%s want 180s", DefaultHeartbeatOnly)
	}
	cfg := Config{}.withDefaults()
	if cfg.FirstFrame != DefaultFirstFrame || cfg.Silence != DefaultSilence || cfg.HeartbeatOnly != DefaultHeartbeatOnly {
		t.Fatalf("withDefaults mismatch: %+v", cfg)
	}
}
