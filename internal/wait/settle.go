package wait

import "time"

// Tracker is the settle clock: a success or failure counts only once it has
// held continuously for the settle window. Any change of verdict resets the
// clock, including a detour through pending.
type Tracker struct {
	settle  time.Duration
	started bool
	state   Verdict
	since   time.Time
}

// NewTracker returns a tracker for the given settle window.
func NewTracker(settle time.Duration) *Tracker { return &Tracker{settle: settle} }

// Observe records the verdict seen at now. It reports whether the verdict
// is settled (a success or failure held for the window) and whether it
// differs from the previous observation.
func (t *Tracker) Observe(now time.Time, v Verdict) (settled, changed bool) {
	if !t.started || v != t.state {
		changed = true
		t.started, t.state, t.since = true, v, now
	}
	return t.Settled(now), changed
}

// Settled reports whether the current verdict has held long enough.
func (t *Tracker) Settled(now time.Time) bool {
	return t.started && t.state != Pending && now.Sub(t.since) >= t.settle
}

// State is the most recently observed verdict.
func (t *Tracker) State() Verdict { return t.state }

// Held is how long the current verdict has held.
func (t *Tracker) Held(now time.Time) time.Duration {
	if !t.started {
		return 0
	}
	return now.Sub(t.since)
}

// Deadline is when an unsettled success or failure settles if nothing
// changes. ok is false when there is nothing to settle.
func (t *Tracker) Deadline() (at time.Time, ok bool) {
	if !t.started || t.state == Pending {
		return time.Time{}, false
	}
	return t.since.Add(t.settle), true
}

// Settle is the configured window.
func (t *Tracker) Settle() time.Duration { return t.settle }
