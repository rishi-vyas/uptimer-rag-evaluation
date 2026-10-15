package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rishi-vyas/uptimer/internal/check"
	"github.com/rishi-vyas/uptimer/internal/config"
	"github.com/rishi-vyas/uptimer/internal/metrics"
	"github.com/rishi-vyas/uptimer/internal/monitor"
	"github.com/rishi-vyas/uptimer/internal/store"
)

func newTestServer(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	targets := []config.Target{
		{Name: "api", Type: config.TypeHTTP, URL: "https://example.com", Method: "GET",
			Interval: config.Duration(time.Minute), Timeout: config.Duration(time.Second), FailureThreshold: 1, RecoveryThreshold: 1, SLO: 99.9},
		{Name: "db", Type: config.TypeTCP, Address: "db:5432",
			Interval: config.Duration(time.Minute), Timeout: config.Duration(time.Second), FailureThreshold: 1, RecoveryThreshold: 1, SLO: 99.9},
	}
	st := store.New(50)
	reg := metrics.NewUptimerRegistry("test")
	m, err := monitor.New(monitor.Options{Targets: targets, Store: st, Metrics: reg,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Monitor: m, Store: st, Metrics: reg}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, st
}

func get(t *testing.T, url string) (int, string, http.Header) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

func TestEndpoints(t *testing.T) {
	ts, st := newTestServer(t)
	now := time.Now()
	for i := 0; i < 5; i++ {
		st.Record(check.Result{Target: "api", CheckedAt: now.Add(time.Duration(i) * time.Second), OK: true, Latency: 42 * time.Millisecond})
	}

	if code, body, _ := get(t, ts.URL+"/healthz"); code != 200 || body != "ok\n" {
		t.Errorf("healthz: %d %q", code, body)
	}

	code, body, hdr := get(t, ts.URL+"/api/status")
	if code != 200 || !strings.HasPrefix(hdr.Get("Content-Type"), "application/json") {
		t.Fatalf("status: %d %s", code, hdr.Get("Content-Type"))
	}
	var sr struct {
		Overall string `json:"overall"`
		Targets []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
			Stats  struct {
				Samples int `json:"samples"`
			} `json:"stats"`
		} `json:"targets"`
	}
	if err := json.Unmarshal([]byte(body), &sr); err != nil {
		t.Fatal(err)
	}
	if sr.Overall != "unknown" || len(sr.Targets) != 2 || sr.Targets[0].Name != "api" || sr.Targets[0].Stats.Samples != 5 {
		t.Errorf("status body: %+v", sr)
	}

	code, body, _ = get(t, ts.URL+"/api/targets/api/history?limit=3")
	var hist []map[string]any
	_ = json.Unmarshal([]byte(body), &hist)
	if code != 200 || len(hist) != 3 || hist[0]["latency_ms"].(float64) != 42 {
		t.Errorf("history: %d %v", code, hist)
	}
	if code, _, _ := get(t, ts.URL+"/api/targets/nope/history"); code != 404 {
		t.Errorf("unknown target: %d", code)
	}
	if code, _, _ := get(t, ts.URL+"/api/targets/api/history?limit=0"); code != 400 {
		t.Errorf("bad limit: %d", code)
	}

	code, body, hdr = get(t, ts.URL+"/metrics")
	if code != 200 || !strings.Contains(hdr.Get("Content-Type"), "version=0.0.4") ||
		!strings.Contains(body, `uptimer_target_status{target="db",status="unknown"} 1`) {
		t.Errorf("metrics: %d %s\n%s", code, hdr.Get("Content-Type"), body)
	}

	code, body, _ = get(t, ts.URL+"/")
	if code != 200 || !strings.Contains(body, "<title>Uptimer Status</title>") {
		t.Errorf("status page: %d", code)
	}
}

func TestOverall(t *testing.T) {
	mk := func(ss ...monitor.Status) []monitor.TargetStatus {
		var out []monitor.TargetStatus
		for _, s := range ss {
			out = append(out, monitor.TargetStatus{Status: s})
		}
		return out
	}
	cases := []struct {
		in   []monitor.TargetStatus
		want monitor.Status
	}{
		{mk(monitor.StatusUp, monitor.StatusUp), monitor.StatusUp},
		{mk(monitor.StatusUp, monitor.StatusDegraded), monitor.StatusDegraded},
		{mk(monitor.StatusDegraded, monitor.StatusDown, monitor.StatusUnknown), monitor.StatusDown},
		{mk(monitor.StatusUp, monitor.StatusUnknown), monitor.StatusUnknown},
	}
	for _, c := range cases {
		if got := overall(c.in); got != c.want {
			t.Errorf("overall(%v) = %s, want %s", c.in, got, c.want)
		}
	}
}
