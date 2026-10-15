package metrics

import (
	"strings"
	"testing"
)

func render(r *Registry) string {
	var b strings.Builder
	_, _ = r.WriteTo(&b)
	return b.String()
}

func TestExposition(t *testing.T) {
	r := NewRegistry()
	r.NewGauge("g", "a gauge", "target")
	r.NewCounter("c_total", "a counter", "target", "result")
	r.NewHistogram("h_seconds", "a histogram", []float64{0.5, 0.1, 1}, "target")

	r.Set("g", 1.5, "api")
	r.Inc("c_total", "api", "success")
	r.Inc("c_total", "api", "success")
	r.Inc("c_total", "api", "failure")
	for _, v := range []float64{0.05, 0.3, 0.3, 0.7, 3} {
		r.Observe("h_seconds", v, "api")
	}

	out := render(r)
	for _, want := range []string{
		"# HELP g a gauge\n# TYPE g gauge\ng{target=\"api\"} 1.5\n",
		"# TYPE c_total counter\n",
		`c_total{target="api",result="failure"} 1`,
		`c_total{target="api",result="success"} 2`,
		"# TYPE h_seconds histogram\n",
		`h_seconds_bucket{target="api",le="0.1"} 1`,
		`h_seconds_bucket{target="api",le="0.5"} 3`,
		`h_seconds_bucket{target="api",le="1"} 4`,
		`h_seconds_bucket{target="api",le="+Inf"} 5`,
		`h_seconds_sum{target="api"} 4.35`,
		`h_seconds_count{target="api"} 5`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestLabelEscaping(t *testing.T) {
	r := NewRegistry()
	r.NewGauge("g", "help with \\ and\nnewline", "target")
	r.Set("g", 1, "we\"ird\\name\n")
	out := render(r)
	if !strings.Contains(out, `g{target="we\"ird\\name\n"} 1`) {
		t.Errorf("label not escaped:\n%s", out)
	}
	if !strings.Contains(out, `# HELP g help with \\ and\nnewline`) {
		t.Errorf("help not escaped:\n%s", out)
	}
}

func TestDelete(t *testing.T) {
	r := NewRegistry()
	r.NewGauge("s", "status", "target", "status")
	r.Set("s", 1, "api", "up")
	r.Delete("s", "api", "up")
	r.Set("s", 1, "api", "down")
	out := render(r)
	if strings.Contains(out, `status="up"`) || !strings.Contains(out, `status="down"`) {
		t.Errorf("delete failed:\n%s", out)
	}
}

func TestMisuse(t *testing.T) {
	r := NewRegistry()
	r.NewGauge("g", "", "a")
	mustPanic(t, func() { r.Set("g", 1) })
	mustPanic(t, func() { r.Set("missing", 1) })
	mustPanic(t, func() { r.NewGauge("g", "") })
}

func mustPanic(t *testing.T, f func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Error("expected panic")
		}
	}()
	f()
}

func TestUptimerRegistry(t *testing.T) {
	out := render(NewUptimerRegistry("v1.2.3"))
	if !strings.Contains(out, `uptimer_build_info{version="v1.2.3"} 1`) {
		t.Errorf("build info missing:\n%s", out)
	}
}
