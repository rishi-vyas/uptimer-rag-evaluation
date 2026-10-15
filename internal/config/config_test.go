package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseAppliesDefaults(t *testing.T) {
	c, err := Parse([]byte(`{"targets":[{"name":"api","type":"HTTP","url":"https://example.com/health"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	tg := c.Targets[0]
	if c.Listen != DefaultListen || c.HistorySize != DefaultHistorySize {
		t.Errorf("root defaults not applied: %+v", c)
	}
	if tg.Type != TypeHTTP || tg.Method != "GET" {
		t.Errorf("type/method not normalised: %q %q", tg.Type, tg.Method)
	}
	if tg.Interval.D() != DefaultInterval || tg.Timeout.D() != DefaultTimeout {
		t.Errorf("interval/timeout defaults: %s %s", tg.Interval.D(), tg.Timeout.D())
	}
	if tg.FailureThreshold != 3 || tg.RecoveryThreshold != 2 || tg.SLO != 99.9 {
		t.Errorf("threshold defaults: %+v", tg)
	}
	if tg.CertExpiryWarning.D() != DefaultCertExpiryWarning {
		t.Errorf("cert warning default: %s", tg.CertExpiryWarning.D())
	}
}

func TestTimeoutDefaultNeverExceedsInterval(t *testing.T) {
	c, err := Parse([]byte(`{"targets":[{"name":"db","type":"tcp","address":"db:5432","interval":"2s"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Targets[0].Timeout.D(); got != 2*time.Second {
		t.Errorf("timeout = %s, want 2s", got)
	}
}

func TestValidateCollectsAllErrors(t *testing.T) {
	_, err := Parse([]byte(`{
	  "targets": [
	    {"name":"a","type":"http","url":"not a url"},
	    {"name":"a","type":"tcp","address":"no-port"},
	    {"name":"c","type":"smtp"},
	    {"name":"d","type":"dns","host":"x","slo":100},
	    {"name":"e","type":"dns","host":"x","interval":"1s","timeout":"5s"}
	  ],
	  "alerts": {"webhooks":[{"url":"ftp://x","format":"teams"}]}
	}`))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{
		"url must be an absolute",
		"duplicate name",
		"address must be host:port",
		"type must be one of",
		"slo must be between",
		"timeout must be positive and no longer than interval",
		"format must be slack or generic",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in:\n%v", want, err)
		}
	}
}

func TestRejectsUnknownFields(t *testing.T) {
	_, err := Parse([]byte(`{"targets":[{"name":"a","type":"dns","host":"x","intervall":"5s"}]}`))
	if err == nil || !strings.Contains(err.Error(), "intervall") {
		t.Fatalf("expected unknown-field error, got %v", err)
	}
}

func TestDurationFormats(t *testing.T) {
	c, err := Parse([]byte(`{"targets":[{"name":"a","type":"dns","host":"x","interval":"1m30s","timeout":2000000000}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Targets[0].Interval.D() != 90*time.Second || c.Targets[0].Timeout.D() != 2*time.Second {
		t.Errorf("got %s / %s", c.Targets[0].Interval.D(), c.Targets[0].Timeout.D())
	}
	if _, err := Parse([]byte(`{"targets":[{"name":"a","type":"dns","host":"x","interval":"soon"}]}`)); err == nil {
		t.Error("expected bad duration error")
	}
}

func TestLoadExpandsEnv(t *testing.T) {
	t.Setenv("SLACK_URL", "https://hooks.example.com/abc")
	p := filepath.Join(t.TempDir(), "c.json")
	body := `{"targets":[{"name":"a","type":"dns","host":"x"}],"alerts":{"webhooks":[{"url":"${SLACK_URL}","format":"slack"}]}}`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Alerts.Webhooks[0].URL; got != "https://hooks.example.com/abc" {
		t.Errorf("env not expanded: %q", got)
	}
}

func TestExampleConfigIsValid(t *testing.T) {
	t.Setenv("SLACK_WEBHOOK_URL", "https://hooks.slack.com/services/T000/B000/XXX")
	if _, err := Load("../../config.example.json"); err != nil {
		t.Fatalf("config.example.json must stay valid: %v", err)
	}
}
