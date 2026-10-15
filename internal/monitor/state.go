package monitor

import (
	"time"

	"github.com/rishi-vyas/uptimer/internal/check"
)

// Status is a target's health as seen by the state machine.
type Status string

const (
	StatusUnknown  Status = "unknown"
	StatusUp       Status = "up"
	StatusDegraded Status = "degraded"
	StatusDown     Status = "down"
)

// Transition is emitted whenever a target changes status.
type Transition struct {
	From     Status
	To       Status
	At       time.Time
	Previous time.Duration // how long the target spent in From
	Result   check.Result
}

// Tracker is a per-target state machine with hysteresis:
//
//   - A target only goes DOWN after FailureThreshold consecutive failures, so a
//     single dropped packet does not page anyone.
//   - A DOWN target only comes back after RecoveryThreshold consecutive
//     successes, so a flapping service does not spam up/down alerts.
//   - UP <-> DEGRADED moves immediately: it is informational, not paging.
//
// Tracker is not safe for concurrent use; the owning runner serialises access.
type Tracker struct {
	failThreshold    int
	recoverThreshold int

	status      Status
	since       time.Time
	consecFail  int
	consecOK    int
	lastAlertAt time.Time
}

// NewTracker creates a tracker in the UNKNOWN state.
func NewTracker(failThreshold, recoverThreshold int) *Tracker {
	return &Tracker{
		failThreshold:    max(1, failThreshold),
		recoverThreshold: max(1, recoverThreshold),
		status:           StatusUnknown,
	}
}

// Observe feeds one result in and returns a transition if the status changed.
func (t *Tracker) Observe(r check.Result) (Transition, bool) {
	if t.since.IsZero() {
		t.since = r.CheckedAt
	}
	next := t.status
	if r.OK {
		t.consecFail = 0
		t.consecOK++
		healthy := StatusUp
		if r.Degraded {
			healthy = StatusDegraded
		}
		if t.status != StatusDown || t.consecOK >= t.recoverThreshold {
			next = healthy
		}
	} else {
		t.consecOK = 0
		t.consecFail++
		if t.consecFail >= t.failThreshold {
			next = StatusDown
		}
	}

	if next == t.status {
		return Transition{}, false
	}
	tr := Transition{
		From:     t.status,
		To:       next,
		At:       r.CheckedAt,
		Previous: r.CheckedAt.Sub(t.since),
		Result:   r,
	}
	t.status = next
	t.since = r.CheckedAt
	return tr, true
}

// Status returns the current status and when it began.
func (t *Tracker) Status() (Status, time.Time) { return t.status, t.since }

// ConsecutiveFailures is the current failure streak.
func (t *Tracker) ConsecutiveFailures() int { return t.consecFail }

// ReminderDue reports whether a repeat DOWN alert should be sent now.
func (t *Tracker) ReminderDue(now time.Time, every time.Duration) bool {
	if every <= 0 || t.status != StatusDown {
		return false
	}
	last := t.lastAlertAt
	if last.Before(t.since) {
		last = t.since
	}
	return now.Sub(last) >= every
}

// MarkAlerted records that an alert went out at now.
func (t *Tracker) MarkAlerted(now time.Time) { t.lastAlertAt = now }
