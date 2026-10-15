package monitor

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rishi-vyas/uptimer/internal/alert"
	"github.com/rishi-vyas/uptimer/internal/check"
	"github.com/rishi-vyas/uptimer/internal/config"
	"github.com/rishi-vyas/uptimer/internal/metrics"
	"github.com/rishi-vyas/uptimer/internal/store"
)

type collector struct {
	mu     sync.Mutex
	events []alert.Event
}

func (c *collector) Send(e alert.Event) { c.mu.Lock(); c.events = append(c.events, e); c.mu.Unlock() }
func (c *collector) snapshot() []alert.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]alert.Event(nil), c.events...)
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestMonitorEndToEnd(t *testing.T) {
	var healthy atomic.Bool
	healthy.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !healthy.Load() {
			w.WriteHeader(http.StatusBadGateway)
		}
	}))
	defer srv.Close()

	target := config.Target{
		Name: "api", Type: config.TypeHTTP, URL: srv.URL, Method: "GET",
		Interval: config.Duration(30 * time.Millisecond), Timeout: config.Duration(30 * time.Millisecond),
		FailureThreshold: 2, RecoveryThreshold: 2, SLO: 99,
	}
	st := store.New(100)
	reg := metrics.NewUptimerRegistry("test")
	alerts := &collector{}
	m, err := New(Options{
		Targets: []config.Target{target}, Store: st, Metrics: reg, Alerter: alerts,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), NoJitter: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()

	status := func() Status { return m.Snapshot()[0].Status }
	eventually(t, "up", func() bool { return status() == StatusUp })
	if n := len(alerts.snapshot()); n != 0 {
		t.Fatalf("startup UNKNOWN->UP should not alert, got %d events", n)
	}

	healthy.Store(false)
	eventually(t, "down", func() bool { return status() == StatusDown })
	healthy.Store(true)
	eventually(t, "recovery", func() bool { return status() == StatusUp })

	cancel()
	<-done

	ev := alerts.snapshot()
	if len(ev) != 2 || ev[0].To != "down" || ev[1].To != "up" || ev[1].From != "down" {
		t.Fatalf("alerts = %+v, want [down, up]", ev)
	}
	if !strings.Contains(ev[0].Reason, "502") || ev[0].Endpoint != srv.URL {
		t.Errorf("down event lacks detail: %+v", ev[0])
	}

	snap := m.Snapshot()[0]
	if snap.Stats.Samples < 5 || snap.Stats.Availability >= 1 || snap.Last == nil {
		t.Errorf("snapshot stats look wrong: %+v", snap)
	}

	var b strings.Builder
	_, _ = reg.WriteTo(&b)
	out := b.String()
	for _, want := range []string{
		`uptimer_up{target="api",type="http"} 1`,
		`uptimer_target_status{target="api",status="up"} 1`,
		`uptimer_state_transitions_total{target="api",to="down"} 1`,
		`uptimer_checks_total{target="api",result="failure"}`,
		`uptimer_check_duration_seconds_count{target="api",type="http"}`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics missing %q", want)
		}
	}
	if strings.Contains(out, `status="down"} 1`) || strings.Contains(out, `status="unknown"`) {
		t.Errorf("stale one-hot status series left behind:\n%s", out)
	}
}

func TestReminderAlerts(t *testing.T) {
	alerts := &collector{}
	target := config.Target{Name: "db", Type: config.TypeTCP, Address: "127.0.0.1:1",
		Interval: config.Duration(time.Minute), Timeout: config.Duration(time.Second),
		FailureThreshold: 1, RecoveryThreshold: 1, SLO: 99}
	m, err := New(Options{Targets: []config.Target{target}, Alerter: alerts, RepeatInterval: 10 * time.Minute,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	r := m.runners[0]
	t0 := time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)
	for i := 0; i <= 25; i++ { // one failing check per minute for 25 minutes
		r.record(check.Result{Target: "db", CheckedAt: t0.Add(time.Duration(i) * time.Minute), Reason: "connection refused"})
	}
	ev := alerts.snapshot()
	// initial DOWN at 0, reminders at 10 and 20
	if len(ev) != 3 || ev[0].Reminder || !ev[1].Reminder || !ev[2].Reminder {
		t.Fatalf("events = %+v", ev)
	}
	if !ev[2].At.Equal(t0.Add(20 * time.Minute)) {
		t.Errorf("second reminder at %s", ev[2].At)
	}
}

func TestShutdownDoesNotRecordInterruptedProbe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()
	st := store.New(10)
	alerts := &collector{}
	target := config.Target{Name: "slow", Type: config.TypeHTTP, URL: srv.URL, Method: "GET",
		Interval: config.Duration(time.Second), Timeout: config.Duration(time.Second),
		FailureThreshold: 1, RecoveryThreshold: 1, SLO: 99}
	m, _ := New(Options{Targets: []config.Target{target}, Store: st, Alerter: alerts, NoJitter: true,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done
	if n := len(st.History("slow", 0)); n != 0 || len(alerts.snapshot()) != 0 {
		t.Errorf("interrupted probe was recorded: history=%d alerts=%d", n, len(alerts.snapshot()))
	}
}
