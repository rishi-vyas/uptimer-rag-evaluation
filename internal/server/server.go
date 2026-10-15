// Package server exposes the status page, JSON API, health check and metrics.
package server

import (
	"embed"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/rishi-vyas/uptimer/internal/metrics"
	"github.com/rishi-vyas/uptimer/internal/monitor"
	"github.com/rishi-vyas/uptimer/internal/store"
)

//go:embed web
var webFS embed.FS

// Server bundles the HTTP handlers.
type Server struct {
	Monitor *monitor.Monitor
	Store   *store.Store
	Metrics *metrics.Registry
	Logger  *slog.Logger
	// BeforeScrape runs before /metrics renders (e.g. to refresh gauges).
	BeforeScrape func()
}

// Handler returns the routed, logged http.Handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(webFS, "web")
	mux.Handle("GET /", http.FileServerFS(static))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /api/status", s.status)
	mux.HandleFunc("GET /api/targets/{name}/history", s.history)
	mux.HandleFunc("GET /metrics", s.metrics)
	return s.logRequests(mux)
}

type statusResponse struct {
	GeneratedAt time.Time              `json:"generated_at"`
	Overall     monitor.Status         `json:"overall"`
	Targets     []monitor.TargetStatus `json:"targets"`
}

func (s *Server) status(w http.ResponseWriter, _ *http.Request) {
	targets := s.Monitor.Snapshot()
	writeJSON(w, http.StatusOK, statusResponse{
		GeneratedAt: time.Now().UTC(),
		Overall:     overall(targets),
		Targets:     targets,
	})
}

// overall is the worst status across targets (down > degraded > unknown > up).
func overall(ts []monitor.TargetStatus) monitor.Status {
	rank := map[monitor.Status]int{monitor.StatusUp: 0, monitor.StatusUnknown: 1, monitor.StatusDegraded: 2, monitor.StatusDown: 3}
	worst := monitor.StatusUp
	for _, t := range ts {
		if rank[t.Status] > rank[worst] {
			worst = t.Status
		}
	}
	return worst
}

type historyPoint struct {
	At         time.Time `json:"at"`
	OK         bool      `json:"ok"`
	Degraded   bool      `json:"degraded"`
	LatencyMS  float64   `json:"latency_ms"`
	StatusCode int       `json:"status_code,omitempty"`
	Reason     string    `json:"reason,omitempty"`
}

func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !s.Monitor.HasTarget(name) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown target"})
		return
	}
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 10000 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "limit must be 1..10000"})
			return
		}
		limit = n
	}
	hist := s.Store.History(name, limit)
	out := make([]historyPoint, len(hist))
	for i, h := range hist {
		out[i] = historyPoint{
			At: h.CheckedAt, OK: h.OK, Degraded: h.Degraded,
			LatencyMS:  float64(h.Latency.Microseconds()) / 1000,
			StatusCode: h.StatusCode, Reason: h.Reason,
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) metrics(w http.ResponseWriter, _ *http.Request) {
	if s.BeforeScrape != nil {
		s.BeforeScrape()
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = s.Metrics.WriteTo(w)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.code = code
	r.ResponseWriter.WriteHeader(code)
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(rec, r)
		if s.Logger != nil && r.URL.Path != "/healthz" && r.URL.Path != "/metrics" {
			s.Logger.Debug("http", "method", r.Method, "path", r.URL.Path, "status", rec.code, "dur", time.Since(start))
		}
	})
}
