// Package monitor schedules probes, feeds results through the state machine,
// records history and metrics, and raises alerts on transitions.
package monitor

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/rishi-vyas/uptimer/internal/alert"
	"github.com/rishi-vyas/uptimer/internal/check"
	"github.com/rishi-vyas/uptimer/internal/config"
	"github.com/rishi-vyas/uptimer/internal/metrics"
	"github.com/rishi-vyas/uptimer/internal/store"
)

// Alerter receives events; *alert.Dispatcher satisfies it.
type Alerter interface{ Send(alert.Event) }

// Options wires a Monitor's collaborators.
type Options struct {
	Targets        []config.Target
	Store          *store.Store
	Metrics        *metrics.Registry
	Alerter        Alerter
	Logger         *slog.Logger
	RepeatInterval time.Duration
	StatusURL      string
	// NoJitter disables the randomised start offset (used in tests).
	NoJitter bool
}

// Monitor runs one goroutine per target.
type Monitor struct {
	opts    Options
	runners []*runner
}

type runner struct {
	target  config.Target
	checker check.Checker
	m       *Monitor

	mu      sync.RWMutex
	tracker *Tracker
	last    check.Result
}

// New validates targets and builds checkers.
func New(opts Options) (*Monitor, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	m := &Monitor{opts: opts}
	for _, t := range opts.Targets {
		c, err := check.New(t)
		if err != nil {
			return nil, err
		}
		m.runners = append(m.runners, &runner{
			target:  t,
			checker: c,
			m:       m,
			tracker: NewTracker(t.FailureThreshold, t.RecoveryThreshold),
		})
		if opts.Metrics != nil {
			opts.Metrics.Set(metrics.Status, 1, t.Name, string(StatusUnknown))
			opts.Metrics.Set(metrics.Up, 0, t.Name, t.Type)
		}
	}
	return m, nil
}

// Run blocks until ctx is cancelled and every in-flight probe has finished.
func (m *Monitor) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, r := range m.runners {
		wg.Add(1)
		go func(r *runner) {
			defer wg.Done()
			r.loop(ctx)
		}(r)
	}
	wg.Wait()
}

func (r *runner) loop(ctx context.Context) {
	interval := r.target.Interval.D()
	// Spread first probes across the interval so N targets configured with the
	// same interval don't all fire on the same tick (thundering herd).
	if !r.m.opts.NoJitter {
		delay := time.Duration(rand.Int64N(int64(interval)))
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		r.probe(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (r *runner) probe(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, r.target.Timeout.D())
	res := r.checker.Check(ctx)
	cancel()
	// A probe interrupted by shutdown says nothing about the target, so
	// don't let it trip the state machine or fire a bogus DOWN alert.
	if parent.Err() != nil && !res.OK {
		return
	}
	r.record(res)
}

// record is split from probe so tests can drive the pipeline with synthetic results.
func (r *runner) record(res check.Result) {
	o := r.m.opts
	t := r.target
	if o.Store != nil {
		o.Store.Record(res)
	}

	r.mu.Lock()
	r.last = res
	tr, changed := r.tracker.Observe(res)
	status, _ := r.tracker.Status()
	reminder := !changed && r.tracker.ReminderDue(res.CheckedAt, o.RepeatInterval)
	if changed || reminder {
		r.tracker.MarkAlerted(res.CheckedAt)
	}
	r.mu.Unlock()

	if reg := o.Metrics; reg != nil {
		outcome := "success"
		if !res.OK {
			outcome = "failure"
		}
		reg.Inc(metrics.ChecksTotal, t.Name, outcome)
		reg.Observe(metrics.CheckDuration, res.Latency.Seconds(), t.Name, t.Type)
		if !res.CertExpiry.IsZero() {
			reg.Set(metrics.CertExpiry, float64(res.CertExpiry.Unix()), t.Name)
		}
		up := 0.0
		if status == StatusUp || status == StatusDegraded {
			up = 1
		}
		reg.Set(metrics.Up, up, t.Name, t.Type)
		if o.Store != nil {
			st := o.Store.Stats(t.Name, t.SLO)
			reg.Set(metrics.Availability, st.Availability, t.Name)
			reg.Set(metrics.ErrorBudget, st.ErrorBudgetRemaining, t.Name)
		}
		if changed {
			reg.Inc(metrics.Transitions, t.Name, string(tr.To))
			reg.Delete(metrics.Status, t.Name, string(tr.From))
			reg.Set(metrics.Status, 1, t.Name, string(tr.To))
		}
	}

	if changed {
		o.Logger.Info("status change", "target", t.Name, "from", tr.From, "to", tr.To,
			"reason", res.Reason, "latency_ms", float64(res.Latency.Microseconds())/1000)
		// UNKNOWN -> UP at startup is not news; everything else is.
		if !(tr.From == StatusUnknown && tr.To == StatusUp) && o.Alerter != nil {
			o.Alerter.Send(r.event(tr.From, tr.To, tr.Previous, res, false))
		}
	} else if reminder && o.Alerter != nil {
		o.Alerter.Send(r.event(StatusDown, StatusDown, 0, res, true))
	}
}

func (r *runner) event(from, to Status, prev time.Duration, res check.Result, reminder bool) alert.Event {
	return alert.Event{
		Target:    r.target.Name,
		Endpoint:  r.target.Endpoint(),
		From:      string(from),
		To:        string(to),
		At:        res.CheckedAt,
		Reason:    res.Reason,
		Latency:   res.Latency,
		Duration:  prev,
		Reminder:  reminder,
		StatusURL: r.m.opts.StatusURL,
	}
}

// TargetStatus is the public view of one target, served by the API.
type TargetStatus struct {
	Name                string      `json:"name"`
	Type                string      `json:"type"`
	Endpoint            string      `json:"endpoint"`
	Status              Status      `json:"status"`
	Since               time.Time   `json:"since"`
	ConsecutiveFailures int         `json:"consecutive_failures"`
	SLO                 float64     `json:"slo"`
	IntervalSeconds     float64     `json:"interval_seconds"`
	Last                *LastCheck  `json:"last_check,omitempty"`
	Stats               store.Stats `json:"stats"`
}

// LastCheck is the most recent probe, with latency in milliseconds for humans.
type LastCheck struct {
	At         time.Time  `json:"at"`
	OK         bool       `json:"ok"`
	LatencyMS  float64    `json:"latency_ms"`
	StatusCode int        `json:"status_code,omitempty"`
	Reason     string     `json:"reason,omitempty"`
	CertExpiry *time.Time `json:"cert_expiry,omitempty"`
}

// Snapshot returns the current view of every target in config order.
func (m *Monitor) Snapshot() []TargetStatus {
	out := make([]TargetStatus, 0, len(m.runners))
	for _, r := range m.runners {
		r.mu.RLock()
		status, since := r.tracker.Status()
		ts := TargetStatus{
			Name:                r.target.Name,
			Type:                r.target.Type,
			Endpoint:            r.target.Endpoint(),
			Status:              status,
			Since:               since,
			ConsecutiveFailures: r.tracker.ConsecutiveFailures(),
			SLO:                 r.target.SLO,
			IntervalSeconds:     r.target.Interval.D().Seconds(),
		}
		if !r.last.CheckedAt.IsZero() {
			lc := &LastCheck{
				At:         r.last.CheckedAt,
				OK:         r.last.OK,
				LatencyMS:  float64(r.last.Latency.Microseconds()) / 1000,
				StatusCode: r.last.StatusCode,
				Reason:     r.last.Reason,
			}
			if !r.last.CertExpiry.IsZero() {
				ce := r.last.CertExpiry
				lc.CertExpiry = &ce
			}
			ts.Last = lc
		}
		r.mu.RUnlock()
		if m.opts.Store != nil {
			ts.Stats = m.opts.Store.Stats(r.target.Name, r.target.SLO)
		}
		out = append(out, ts)
	}
	return out
}

// HasTarget reports whether name is configured.
func (m *Monitor) HasTarget(name string) bool {
	for _, r := range m.runners {
		if r.target.Name == name {
			return true
		}
	}
	return false
}
