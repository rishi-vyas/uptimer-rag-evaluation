// Package store keeps a bounded, in-memory history of check results per target
// and derives availability, latency percentiles and SLO error budget from it.
package store

import (
	"math"
	"slices"
	"sync"
	"time"

	"github.com/rishi-vyas/uptimer/internal/check"
)

// ring is a fixed-capacity circular buffer; once full, the oldest entry is overwritten.
type ring struct {
	buf  []check.Result
	next int
	full bool
}

func (r *ring) add(res check.Result) {
	r.buf[r.next] = res
	r.next = (r.next + 1) % len(r.buf)
	if r.next == 0 {
		r.full = true
	}
}

// ordered returns entries oldest-first.
func (r *ring) ordered() []check.Result {
	if !r.full {
		return slices.Clone(r.buf[:r.next])
	}
	out := make([]check.Result, 0, len(r.buf))
	out = append(out, r.buf[r.next:]...)
	return append(out, r.buf[:r.next]...)
}

// Store is safe for concurrent use.
type Store struct {
	mu       sync.RWMutex
	capacity int
	rings    map[string]*ring
}

// New creates a store that keeps the last capacity results per target.
func New(capacity int) *Store {
	return &Store{capacity: max(1, capacity), rings: map[string]*ring{}}
}

// Record appends a result.
func (s *Store) Record(res check.Result) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rings[res.Target]
	if !ok {
		r = &ring{buf: make([]check.Result, s.capacity)}
		s.rings[res.Target] = r
	}
	r.add(res)
}

// History returns up to limit most recent results, oldest first. limit <= 0 means all.
func (s *Store) History(target string, limit int) []check.Result {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.rings[target]
	if !ok {
		return nil
	}
	all := r.ordered()
	if limit > 0 && len(all) > limit {
		all = all[len(all)-limit:]
	}
	return all
}

// Stats summarises the retained window for one target.
type Stats struct {
	Samples      int           `json:"samples"`
	Window       time.Duration `json:"window_ns"`
	Availability float64       `json:"availability"` // 0..1; degraded counts as available
	P50          time.Duration `json:"p50_ns"`
	P95          time.Duration `json:"p95_ns"`
	P99          time.Duration `json:"p99_ns"`
	// ErrorBudgetRemaining is the fraction (can go negative) of the SLO's
	// allowed failures not yet consumed in the window.
	ErrorBudgetRemaining float64 `json:"error_budget_remaining"`
}

// Stats computes availability, latency percentiles of successful checks and
// remaining error budget against slo (a percentage such as 99.9).
func (s *Store) Stats(target string, slo float64) Stats {
	hist := s.History(target, 0)
	st := Stats{Samples: len(hist), Availability: 1, ErrorBudgetRemaining: 1}
	if len(hist) == 0 {
		return st
	}
	st.Window = hist[len(hist)-1].CheckedAt.Sub(hist[0].CheckedAt)

	ok := 0
	lat := make([]time.Duration, 0, len(hist))
	for _, h := range hist {
		if h.OK {
			ok++
			lat = append(lat, h.Latency)
		}
	}
	st.Availability = float64(ok) / float64(len(hist))
	st.ErrorBudgetRemaining = ErrorBudgetRemaining(st.Availability, slo)
	if len(lat) > 0 {
		slices.Sort(lat)
		st.P50 = percentile(lat, 0.50)
		st.P95 = percentile(lat, 0.95)
		st.P99 = percentile(lat, 0.99)
	}
	return st
}

// ErrorBudgetRemaining returns 1 - (observed failure rate / allowed failure rate).
// 1.0 = untouched budget, 0 = exactly exhausted, negative = SLO breached.
func ErrorBudgetRemaining(availability, slo float64) float64 {
	allowed := 1 - slo/100
	if allowed <= 0 {
		return 0
	}
	return 1 - (1-availability)/allowed
}

// percentile uses the nearest-rank method on an ascending slice.
func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(float64(len(sorted))*p-1e-9)) - 1
	idx = max(0, min(idx, len(sorted)-1))
	return sorted[idx]
}
