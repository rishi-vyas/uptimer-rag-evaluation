package store

import (
	"math"
	"sync"
	"testing"
	"time"

	"github.com/rishi-vyas/uptimer/internal/check"
)

var t0 = time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)

func res(i int, ok bool, ms int) check.Result {
	return check.Result{Target: "a", CheckedAt: t0.Add(time.Duration(i) * time.Minute), OK: ok, Latency: time.Duration(ms) * time.Millisecond}
}

func TestRingWrapsKeepingNewest(t *testing.T) {
	s := New(3)
	for i := 0; i < 5; i++ {
		s.Record(res(i, true, i))
	}
	h := s.History("a", 0)
	if len(h) != 3 {
		t.Fatalf("len = %d", len(h))
	}
	for i, want := range []int{2, 3, 4} {
		if h[i].Latency != time.Duration(want)*time.Millisecond {
			t.Errorf("h[%d] = %v, want latency %dms (oldest first)", i, h[i].Latency, want)
		}
	}
	if got := s.History("a", 2); len(got) != 2 || got[1].Latency != 4*time.Millisecond {
		t.Errorf("limit not applied to newest: %+v", got)
	}
	if s.History("missing", 0) != nil {
		t.Error("unknown target should be nil")
	}
}

func TestStats(t *testing.T) {
	s := New(1000)
	// 1000 checks, 1 failure => 99.9% availability.
	for i := 0; i < 1000; i++ {
		s.Record(res(i, i != 500, i%100+1)) // latencies 1..100ms
	}
	st := s.Stats("a", 99.9)
	if st.Samples != 1000 || math.Abs(st.Availability-0.999) > 1e-9 {
		t.Fatalf("availability = %v over %d", st.Availability, st.Samples)
	}
	if math.Abs(st.ErrorBudgetRemaining) > 1e-6 {
		t.Errorf("budget should be exactly exhausted, got %v", st.ErrorBudgetRemaining)
	}
	// Latency percentiles cover successful checks only: the failed sample (1ms)
	// is excluded, shifting nearest-rank results up by one bucket.
	if st.P50 != 51*time.Millisecond || st.P95 != 96*time.Millisecond || st.P99 != 100*time.Millisecond {
		t.Errorf("percentiles p50=%s p95=%s p99=%s", st.P50, st.P95, st.P99)
	}
	if st.Window != 999*time.Minute {
		t.Errorf("window = %s", st.Window)
	}
}

func TestStatsEmpty(t *testing.T) {
	st := New(10).Stats("nope", 99.9)
	if st.Samples != 0 || st.Availability != 1 || st.ErrorBudgetRemaining != 1 {
		t.Errorf("empty stats should be optimistic: %+v", st)
	}
}

func TestErrorBudget(t *testing.T) {
	cases := []struct{ avail, slo, want float64 }{
		{1, 99.9, 1},
		{0.9995, 99.9, 0.5},
		{0.999, 99.9, 0},
		{0.998, 99.9, -1},
		{0.99, 99, 0},
	}
	for _, c := range cases {
		if got := ErrorBudgetRemaining(c.avail, c.slo); math.Abs(got-c.want) > 1e-6 {
			t.Errorf("ErrorBudgetRemaining(%v, %v) = %v, want %v", c.avail, c.slo, got, c.want)
		}
	}
}

func TestConcurrentAccess(t *testing.T) {
	s := New(100)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				s.Record(res(i, i%7 != 0, g))
				_ = s.Stats("a", 99)
				_ = s.History("a", 10)
			}
		}(g)
	}
	wg.Wait()
	if got := len(s.History("a", 0)); got != 100 {
		t.Errorf("len = %d", got)
	}
}

func TestPercentileNearestRank(t *testing.T) {
	v := make([]time.Duration, 100)
	for i := range v {
		v[i] = time.Duration(i+1) * time.Millisecond
	}
	for p, want := range map[float64]int{0.5: 50, 0.95: 95, 0.99: 99, 1: 100, 0.001: 1} {
		if got := percentile(v, p); got != time.Duration(want)*time.Millisecond {
			t.Errorf("p%v = %s, want %dms", p*100, got, want)
		}
	}
	if percentile(nil, 0.5) != 0 {
		t.Error("empty slice should give 0")
	}
}
