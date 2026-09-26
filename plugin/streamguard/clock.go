package streamguard

import "time"

// Clock is the injectable time source for the Stream Guard watchdog (OpenCodex
// T04 streamHealthClock pattern). Production uses RealClock; unit tests supply
// a ManualClock so re-arm / min(silence, heartbeatOnly) contracts can be
// asserted without racing wall time.
type Clock interface {
	Now() time.Time
	AfterFunc(d time.Duration, f func()) Timer
}

// Timer is a stoppable one-shot scheduled by Clock.AfterFunc.
type Timer interface {
	Stop() bool
}

// RealClock uses the process wall clock and time.AfterFunc.
type RealClock struct{}

func (RealClock) Now() time.Time { return time.Now() }

func (RealClock) AfterFunc(d time.Duration, f func()) Timer {
	return realTimer{t: time.AfterFunc(d, f)}
}

type realTimer struct{ t *time.Timer }

func (r realTimer) Stop() bool { return r.t.Stop() }

// ManualClock is a test clock: time advances only via AdvanceTo, and timers
// fire synchronously from AdvanceTo in deadline order.
type ManualClock struct {
	now     time.Time
	nextID  int
	pending map[int]*manualPending
}

type manualPending struct {
	at time.Time
	f  func()
}

func NewManualClock(start time.Time) *ManualClock {
	return &ManualClock{
		now:     start,
		nextID:  1,
		pending: make(map[int]*manualPending),
	}
}

func (m *ManualClock) Now() time.Time { return m.now }

func (m *ManualClock) AfterFunc(d time.Duration, f func()) Timer {
	id := m.nextID
	m.nextID++
	m.pending[id] = &manualPending{at: m.now.Add(d), f: f}
	return manualTimer{m: m, id: id}
}

type manualTimer struct {
	m  *ManualClock
	id int
}

func (t manualTimer) Stop() bool {
	_, ok := t.m.pending[t.id]
	delete(t.m.pending, t.id)
	return ok
}

// Armed returns how many timers are currently scheduled.
func (m *ManualClock) Armed() int { return len(m.pending) }

// AdvanceTo moves virtual time to target, firing every due timer in deadline order.
func (m *ManualClock) AdvanceTo(target time.Time) {
	for fired := 0; ; fired++ {
		if fired > 1000 {
			panic("streamguard: ManualClock fired 1000 timers without draining — an expired deadline is being re-armed")
		}
		var (
			bestID int
			best   *manualPending
		)
		for id, p := range m.pending {
			if p.at.After(target) {
				continue
			}
			if best == nil || p.at.Before(best.at) || (p.at.Equal(best.at) && id < bestID) {
				bestID, best = id, p
			}
		}
		if best == nil {
			break
		}
		delete(m.pending, bestID)
		if best.at.After(m.now) {
			m.now = best.at
		}
		best.f()
	}
	if target.After(m.now) {
		m.now = target
	}
}
