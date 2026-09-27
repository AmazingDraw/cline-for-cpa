// Package streamguard implements the inbound Stream Guard watchdog for
// cline-for-cpa, modeled on OpenCodex T04 (live-transport.ts) and
// cursor-for-cpa run_turn.go.
//
// Contract:
//  1. firstFrameTimeout — armed at Start; cleared by the first real upstream event.
//  2. After first frame: ONE timer at min(lastInbound+silence, lastMeaningful+heartbeatOnly).
//  3. NoteInbound(livenessOnly) — every real upstream event updates lastInbound;
//     only non-liveness updates lastMeaningful.
//  4. Plugin/host keepalive MUST NOT call NoteInbound (ignored entirely).
//  5. Injectable Clock for unit tests.
//  6. Disarm on expected / clean end.
//  7. Fail via typed StallError distinguishing first-frame / silence / heartbeat-only.
package streamguard

import (
	"fmt"
	"sync"
	"time"
)

// Defaults: first-frame 60s / silence 120s / heartbeat-only 180s.
// Long thinking is covered by heartbeat-only, not by inflating first-frame.
// Silence was raised from 60s to 120s on 2026-09-27: reasoning models
// (glm-5.3-flash was the live case) routinely pause well over a minute
// mid-generation before the next frame, so the old default aborted turns the
// upstream was still actively producing. An explicit config value still wins.
const (
	DefaultFirstFrame    = 60 * time.Second
	DefaultSilence       = 120 * time.Second
	DefaultHeartbeatOnly = 180 * time.Second
)

// Kind classifies which Stream Guard deadline fired.
type Kind string

const (
	KindFirstFrame    Kind = "first_frame_timeout"
	KindSilence       Kind = "stream_silence_timeout"
	KindHeartbeatOnly Kind = "stream_heartbeat_only_timeout"
)

// StallError is the typed failure surfaced through the same envelope path as a
// transport failure (OpenCodex failAndClear / cursor-for-cpa ErrorEnvelopeWithStatus).
type StallError struct {
	Kind          Kind
	SilenceFor    time.Duration
	MeaningfulFor time.Duration
	Message       string
}

func (e *StallError) Error() string { return e.Message }

// Config holds the three Stream Guard budgets.
type Config struct {
	FirstFrame    time.Duration
	Silence       time.Duration
	HeartbeatOnly time.Duration
}

func (c Config) withDefaults() Config {
	if c.FirstFrame <= 0 {
		c.FirstFrame = DefaultFirstFrame
	}
	if c.Silence <= 0 {
		c.Silence = DefaultSilence
	}
	if c.HeartbeatOnly <= 0 {
		c.HeartbeatOnly = DefaultHeartbeatOnly
	}
	return c
}

// Guard watches one upstream stream.
type Guard struct {
	cfg    Config
	clock  Clock
	onFail func(*StallError)

	mu             sync.Mutex
	started        bool
	firstSeen      bool
	disarmed       bool
	lastInbound    time.Time
	lastMeaningful time.Time
	timer          Timer
	failActive     bool
}

// New constructs a Guard. clock nil → RealClock.
func New(cfg Config, clock Clock, onFail func(*StallError)) *Guard {
	if clock == nil {
		clock = RealClock{}
	}
	if onFail == nil {
		onFail = func(*StallError) {}
	}
	return &Guard{cfg: cfg.withDefaults(), clock: clock, onFail: onFail}
}

// Start arms the first-frame timer. Call once when the upstream request begins.
func (g *Guard) Start() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.disarmed || g.started {
		return
	}
	g.started = true
	g.armFirstFrameLocked()
}

// NoteInbound records a real upstream SSE event/frame.
//
// livenessOnly=true  → server ping / SSE comment: refresh silence clock only.
// livenessOnly=false → content / reasoning / tool delta: refresh both clocks.
//
// Plugin-injected keepalives must NOT call this method at all.
func (g *Guard) NoteInbound(livenessOnly bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.disarmed {
		return
	}
	now := g.clock.Now()
	if !g.firstSeen {
		g.firstSeen = true
		g.stopTimerLocked()
		g.lastInbound = now
		g.lastMeaningful = now
		g.failActive = true
		g.armPostFirstLocked()
		return
	}
	g.lastInbound = now
	if !livenessOnly {
		g.lastMeaningful = now
	}
	if g.failActive {
		g.armPostFirstLocked()
	}
}

// Disarm cancels all timers (expected close / clean end / cancel).
func (g *Guard) Disarm() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.disarmed = true
	g.failActive = false
	g.stopTimerLocked()
}

func (g *Guard) stopTimerLocked() {
	if g.timer != nil {
		g.timer.Stop()
		g.timer = nil
	}
}

func (g *Guard) armFirstFrameLocked() {
	g.stopTimerLocked()
	g.timer = g.clock.AfterFunc(g.cfg.FirstFrame, func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		if g.disarmed || g.firstSeen {
			return
		}
		g.disarmed = true
		g.timer = nil
		g.onFail(&StallError{
			Kind:    KindFirstFrame,
			Message: fmt.Sprintf("cline stream stalled: no first upstream frame within %s", g.cfg.FirstFrame),
		})
	})
}

func (g *Guard) armPostFirstLocked() {
	g.stopTimerLocked()
	if g.disarmed || !g.failActive {
		return
	}
	now := g.clock.Now()
	silenceDeadline := g.lastInbound.Add(g.cfg.Silence)
	heartbeatDeadline := g.lastMeaningful.Add(g.cfg.HeartbeatOnly)
	deadline := silenceDeadline
	if heartbeatDeadline.Before(deadline) {
		deadline = heartbeatDeadline
	}
	delay := deadline.Sub(now)
	if delay < 0 {
		delay = 0
	}
	g.timer = g.clock.AfterFunc(delay, g.firePostFirst)
}

func (g *Guard) firePostFirst() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.timer = nil
	if g.disarmed || !g.failActive {
		return
	}
	now := g.clock.Now()
	silenceFor := now.Sub(g.lastInbound)
	meaningfulFor := now.Sub(g.lastMeaningful)
	if silenceFor < g.cfg.Silence && meaningfulFor < g.cfg.HeartbeatOnly {
		g.armPostFirstLocked()
		return
	}
	g.disarmed = true
	g.failActive = false
	if silenceFor < g.cfg.Silence {
		g.onFail(&StallError{
			Kind:          KindHeartbeatOnly,
			SilenceFor:    silenceFor,
			MeaningfulFor: meaningfulFor,
			Message: fmt.Sprintf(
				"cline stream stalled: heartbeat-only traffic for %ds without turn progress",
				int(meaningfulFor.Seconds()),
			),
		})
		return
	}
	g.onFail(&StallError{
		Kind:          KindSilence,
		SilenceFor:    silenceFor,
		MeaningfulFor: meaningfulFor,
		Message: fmt.Sprintf(
			"cline stream stalled: no inbound frames for %ds",
			int(silenceFor.Seconds()),
		),
	})
}

// Snapshot is a test/debug view of guard state.
type Snapshot struct {
	FirstSeen      bool
	Disarmed       bool
	LastInbound    time.Time
	LastMeaningful time.Time
}

// State returns a consistent snapshot (for tests).
func (g *Guard) State() Snapshot {
	g.mu.Lock()
	defer g.mu.Unlock()
	return Snapshot{
		FirstSeen:      g.firstSeen,
		Disarmed:       g.disarmed,
		LastInbound:    g.lastInbound,
		LastMeaningful: g.lastMeaningful,
	}
}
