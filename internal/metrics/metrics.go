// Package metrics exposes probe results in the Prometheus text exposition
// format (v0.0.4). It is a deliberately small, dependency-free implementation
// covering the three metric kinds uptimer needs: gauges, counters, histograms.
package metrics

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultBuckets are latency buckets in seconds.
var DefaultBuckets = []float64{0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

type kind string

const (
	gauge     kind = "gauge"
	counter   kind = "counter"
	histogram kind = "histogram"
)

type family struct {
	name, help string
	kind       kind
	labels     []string
	buckets    []float64
	series     map[string]*series
}

type series struct {
	labelValues []string
	value       float64
	counts      []uint64 // histogram: per-bucket (non-cumulative)
	sum         float64
	count       uint64
}

// Registry holds metric families. It is safe for concurrent use.
type Registry struct {
	mu       sync.Mutex
	families map[string]*family
	order    []string
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{families: map[string]*family{}} }

func (r *Registry) register(name, help string, k kind, labels []string, buckets []float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.families[name]; ok {
		panic("metric registered twice: " + name)
	}
	r.families[name] = &family{name: name, help: help, kind: k, labels: labels, buckets: buckets, series: map[string]*series{}}
	r.order = append(r.order, name)
}

func (r *Registry) get(name string, lv []string) (*family, *series) {
	f, ok := r.families[name]
	if !ok {
		panic("unknown metric: " + name)
	}
	if len(lv) != len(f.labels) {
		panic(fmt.Sprintf("metric %s wants %d labels, got %d", name, len(f.labels), len(lv)))
	}
	key := strings.Join(lv, "\xff")
	s, ok := f.series[key]
	if !ok {
		s = &series{labelValues: append([]string(nil), lv...)}
		if f.kind == histogram {
			s.counts = make([]uint64, len(f.buckets))
		}
		f.series[key] = s
	}
	return f, s
}

// NewGauge registers a gauge.
func (r *Registry) NewGauge(name, help string, labels ...string) {
	r.register(name, help, gauge, labels, nil)
}

// NewCounter registers a counter.
func (r *Registry) NewCounter(name, help string, labels ...string) {
	r.register(name, help, counter, labels, nil)
}

// NewHistogram registers a histogram with the given upper bounds.
func (r *Registry) NewHistogram(name, help string, buckets []float64, labels ...string) {
	b := append([]float64(nil), buckets...)
	sort.Float64s(b)
	r.register(name, help, histogram, labels, b)
}

// Set sets a gauge.
func (r *Registry) Set(name string, v float64, labelValues ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, s := r.get(name, labelValues)
	s.value = v
}

// Inc adds 1 to a counter.
func (r *Registry) Inc(name string, labelValues ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, s := r.get(name, labelValues)
	s.value++
}

// Observe records a histogram sample.
func (r *Registry) Observe(name string, v float64, labelValues ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, s := r.get(name, labelValues)
	for i, ub := range f.buckets {
		if v <= ub {
			s.counts[i]++
			break
		}
	}
	s.sum += v
	s.count++
}

// Delete removes a series (e.g. one-hot status gauges no longer active).
func (r *Registry) Delete(name string, labelValues ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if f, ok := r.families[name]; ok {
		delete(f.series, strings.Join(labelValues, "\xff"))
	}
}

// WriteTo renders every family in exposition format.
func (r *Registry) WriteTo(w io.Writer) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var b strings.Builder
	for _, name := range r.order {
		f := r.families[name]
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", f.name, escapeHelp(f.help), f.name, f.kind)
		keys := make([]string, 0, len(f.series))
		for k := range f.series {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			s := f.series[k]
			if f.kind != histogram {
				fmt.Fprintf(&b, "%s%s %s\n", f.name, labelString(f.labels, s.labelValues, "", ""), formatFloat(s.value))
				continue
			}
			var cum uint64
			for i, ub := range f.buckets {
				cum += s.counts[i]
				fmt.Fprintf(&b, "%s_bucket%s %d\n", f.name, labelString(f.labels, s.labelValues, "le", formatFloat(ub)), cum)
			}
			fmt.Fprintf(&b, "%s_bucket%s %d\n", f.name, labelString(f.labels, s.labelValues, "le", "+Inf"), s.count)
			fmt.Fprintf(&b, "%s_sum%s %s\n", f.name, labelString(f.labels, s.labelValues, "", ""), formatFloat(s.sum))
			fmt.Fprintf(&b, "%s_count%s %d\n", f.name, labelString(f.labels, s.labelValues, "", ""), s.count)
		}
	}
	n, err := io.WriteString(w, b.String())
	return int64(n), err
}

func labelString(names, values []string, extraName, extraValue string) string {
	if len(names) == 0 && extraName == "" {
		return ""
	}
	parts := make([]string, 0, len(names)+1)
	for i, n := range names {
		parts = append(parts, n+`="`+escapeLabel(values[i])+`"`)
	}
	if extraName != "" {
		parts = append(parts, extraName+`="`+extraValue+`"`)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
var helpEscaper = strings.NewReplacer(`\`, `\\`, "\n", `\n`)

func escapeLabel(s string) string { return labelEscaper.Replace(s) }
func escapeHelp(s string) string  { return helpEscaper.Replace(s) }

func formatFloat(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case math.IsNaN(v):
		return "NaN"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// Uptimer metric names.
const (
	Up              = "uptimer_up"
	Status          = "uptimer_target_status"
	CheckDuration   = "uptimer_check_duration_seconds"
	ChecksTotal     = "uptimer_checks_total"
	Transitions     = "uptimer_state_transitions_total"
	CertExpiry      = "uptimer_cert_expiry_timestamp_seconds"
	Availability    = "uptimer_availability_ratio"
	ErrorBudget     = "uptimer_error_budget_remaining_ratio"
	BuildInfo       = "uptimer_build_info"
	AlertsDropped   = "uptimer_alerts_dropped_total"
	StartTimeSecond = "uptimer_start_time_seconds"
)

// NewUptimerRegistry registers every metric uptimer exports.
func NewUptimerRegistry(version string) *Registry {
	r := NewRegistry()
	r.NewGauge(Up, "1 if the target is up or degraded, 0 if down or unknown.", "target", "type")
	r.NewGauge(Status, "One-hot current status of each target.", "target", "status")
	r.NewHistogram(CheckDuration, "Latency of probes in seconds.", DefaultBuckets, "target", "type")
	r.NewCounter(ChecksTotal, "Probes run, by outcome.", "target", "result")
	r.NewCounter(Transitions, "Status changes, by destination status.", "target", "to")
	r.NewGauge(CertExpiry, "Unix time when the target's TLS leaf certificate expires.", "target")
	r.NewGauge(Availability, "Fraction of successful probes in the retained window.", "target")
	r.NewGauge(ErrorBudget, "Fraction of the SLO error budget remaining in the retained window.", "target")
	r.NewGauge(AlertsDropped, "Alert events dropped because the queue was full.")
	r.NewGauge(StartTimeSecond, "Unix time the process started.")
	r.NewGauge(BuildInfo, "Build information.", "version")
	r.Set(BuildInfo, 1, version)
	r.Set(StartTimeSecond, float64(time.Now().Unix()))
	return r
}
