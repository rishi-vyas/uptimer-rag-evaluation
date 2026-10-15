package monitor

import (
	"testing"
	"time"

	"github.com/rishi-vyas/uptimer/internal/check"
)

var t0 = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

// feed drives a tracker with a pattern: '.' ok, 'x' fail, 'd' ok-but-degraded.
// It returns the status after each step and every transition seen.
func feed(tr *Tracker, pattern string) (states []Status, transitions []Transition) {
	for i, c := range pattern {
		r := check.Result{CheckedAt: t0.Add(time.Duration(i) * time.Minute), OK: c != 'x', Degraded: c == 'd'}
		if t, ok := tr.Observe(r); ok {
			transitions = append(transitions, t)
		}
		s, _ := tr.Status()
		states = append(states, s)
	}
	return
}

func TestTrackerTransitions(t *testing.T) {
	cases := []struct {
		name    string
		pattern string
		final   Status
		path    []Status // sequence of "To" statuses
	}{
		{"first success goes up", ".", StatusUp, []Status{StatusUp}},
		{"single blip is ignored", ".x.x.", StatusUp, []Status{StatusUp}},
		{"three failures go down", "...xxx", StatusDown, []Status{StatusUp, StatusDown}},
		{"failures before any success go down", "xxx", StatusDown, []Status{StatusDown}},
		{"one success does not recover", "xxx.", StatusDown, []Status{StatusDown}},
		{"two successes recover", "xxx..", StatusUp, []Status{StatusDown, StatusUp}},
		{"flapping while down stays down", "xxx.x.x.x", StatusDown, []Status{StatusDown}},
		{"degraded is immediate both ways", ".d.", StatusUp, []Status{StatusUp, StatusDegraded, StatusUp}},
		{"recover into degraded", "xxxdd", StatusDegraded, []Status{StatusDown, StatusDegraded}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			states, trs := feed(NewTracker(3, 2), tc.pattern)
			if got := states[len(states)-1]; got != tc.final {
				t.Errorf("final = %s, want %s (states %v)", got, tc.final, states)
			}
			var path []Status
			for _, tr := range trs {
				path = append(path, tr.To)
			}
			if len(path) != len(tc.path) {
				t.Fatalf("transitions %v, want %v", path, tc.path)
			}
			for i := range path {
				if path[i] != tc.path[i] {
					t.Fatalf("transitions %v, want %v", path, tc.path)
				}
			}
		})
	}
}

func TestTrackerRecordsTimeInPreviousState(t *testing.T) {
	_, trs := feed(NewTracker(1, 1), "...x..")
	// up at minute 0, down at minute 3, up at minute 4
	if trs[1].Previous != 3*time.Minute {
		t.Errorf("up duration = %s, want 3m", trs[1].Previous)
	}
	if trs[2].Previous != time.Minute || trs[2].From != StatusDown {
		t.Errorf("down duration = %s from %s, want 1m from down", trs[2].Previous, trs[2].From)
	}
}

func TestReminderDue(t *testing.T) {
	tr := NewTracker(1, 1)
	feed(tr, "x") // down at t0
	tr.MarkAlerted(t0)
	if tr.ReminderDue(t0.Add(4*time.Minute), 5*time.Minute) {
		t.Error("reminder too early")
	}
	if !tr.ReminderDue(t0.Add(5*time.Minute), 5*time.Minute) {
		t.Error("reminder should be due")
	}
	tr.MarkAlerted(t0.Add(5 * time.Minute))
	if tr.ReminderDue(t0.Add(6*time.Minute), 5*time.Minute) {
		t.Error("reminder should reset after alerting")
	}
	if tr.ReminderDue(t0.Add(time.Hour), 0) {
		t.Error("zero interval disables reminders")
	}
}
