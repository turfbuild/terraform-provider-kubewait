package wait

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

func at(d time.Duration) time.Time { return t0.Add(d) }

func TestSettleZero(t *testing.T) {
	tr := NewTracker(0)
	if settled, _ := tr.Observe(at(0), Pending); settled {
		t.Error("pending settled")
	}
	if settled, changed := tr.Observe(at(time.Second), Success); !settled || !changed {
		t.Errorf("settled=%v changed=%v; settle 0 settles on first observation", settled, changed)
	}
}

func TestSettleHold(t *testing.T) {
	tr := NewTracker(2 * time.Minute)
	tr.Observe(at(0), Success)
	if dl, ok := tr.Deadline(); !ok || !dl.Equal(at(2*time.Minute)) {
		t.Errorf("deadline = %v %v", dl, ok)
	}
	if settled, changed := tr.Observe(at(time.Minute), Success); settled || changed {
		t.Errorf("settled=%v changed=%v at 1m", settled, changed)
	}
	if got := tr.Held(at(90 * time.Second)); got != 90*time.Second {
		t.Errorf("held = %s", got)
	}
	if settled, _ := tr.Observe(at(2*time.Minute), Success); !settled {
		t.Error("not settled at 2m")
	}
}

func TestSettleResetThroughPending(t *testing.T) {
	tr := NewTracker(time.Minute)
	tr.Observe(at(0), Success)
	tr.Observe(at(50*time.Second), Pending)
	if _, ok := tr.Deadline(); ok {
		t.Error("pending has no deadline")
	}
	tr.Observe(at(55*time.Second), Success)
	if settled, _ := tr.Observe(at(time.Minute+10*time.Second), Success); settled {
		t.Error("settled 15s after the flip; the clock must reset")
	}
	if settled, _ := tr.Observe(at(time.Minute+55*time.Second), Success); !settled {
		t.Error("not settled a full window after the flip")
	}
}

func TestSettleResetSuccessToFailure(t *testing.T) {
	tr := NewTracker(time.Minute)
	tr.Observe(at(0), Success)
	if _, changed := tr.Observe(at(59*time.Second), Failure); !changed {
		t.Error("expected change")
	}
	if settled, _ := tr.Observe(at(90*time.Second), Failure); settled {
		t.Error("failure settled after 31s")
	}
	if settled, _ := tr.Observe(at(119*time.Second), Failure); !settled || tr.State() != Failure {
		t.Error("failure not settled after 60s")
	}
}

// NVCRE with repeatCount > 1: the controller can move a Certification from
// Failed=True back to InProgress when a child Workflow restarts. A failure
// that does not survive settle must not end the wait.
func TestSettleNVCREFlip(t *testing.T) {
	s := mustParse(t, single(func(r *Raw) { r.Settle = S("2m"); r.Absent = S("failure") }))
	tr := NewTracker(s.Settle)
	inProgress := cr(t, "ns", "c", "Succeeded=False/InProgress", "Failed=False/InProgress")
	failed := cr(t, "ns", "c", "Succeeded=False/InProgress", "Failed=True/CategoryFailed")
	succeeded := cr(t, "ns", "c", "Succeeded=True/AllPassed", "Failed=False/AllPassed")

	steps := []struct {
		at      time.Duration
		obj     map[string]any
		verdict Verdict
		settled bool
	}{
		{0, inProgress, Pending, false},
		{10 * time.Minute, failed, Failure, false},                    // failure clock starts
		{11 * time.Minute, failed, Failure, false},                    // held 1m of 2m
		{11*time.Minute + 30*time.Second, inProgress, Pending, false}, // repeat: back to InProgress
		{12*time.Minute + 30*time.Second, inProgress, Pending, false}, // the old failure window would have closed here
		{20 * time.Minute, succeeded, Success, false},
		{21 * time.Minute, succeeded, Success, false},
		{22 * time.Minute, succeeded, Success, true},
	}
	for i, st := range steps {
		out := Evaluate(s, objs(st.obj))
		if out.Verdict != st.verdict {
			t.Fatalf("step %d: verdict %s, want %s (%s)", i, out.Verdict, st.verdict, out.Reason)
		}
		if settled, _ := tr.Observe(at(st.at), out.Verdict); settled != st.settled {
			t.Fatalf("step %d at %s: settled=%v, want %v", i, st.at, settled, st.settled)
		}
	}

	// The counter-case: Failed held for the full window is a failure.
	tr = NewTracker(s.Settle)
	tr.Observe(at(0), Evaluate(s, objs(inProgress)).Verdict)
	tr.Observe(at(10*time.Minute), Evaluate(s, objs(failed)).Verdict)
	if settled, _ := tr.Observe(at(12*time.Minute), Evaluate(s, objs(failed)).Verdict); !settled || tr.State() != Failure {
		t.Error("Failed held for settle must settle as a failure")
	}
}
