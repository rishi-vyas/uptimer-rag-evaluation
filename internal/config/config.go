// Package config loads and validates uptimer's JSON configuration.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"
)

// Duration wraps time.Duration so it can be written as "30s" or "5m" in JSON.
type Duration time.Duration

// D returns the value as a time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// UnmarshalJSON accepts either a Go duration string ("1m30s") or a number of nanoseconds.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		v, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", s, err)
		}
		*d = Duration(v)
		return nil
	}
	var n int64
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("duration must be a string like \"30s\" or an integer: %s", string(b))
	}
	*d = Duration(n)
	return nil
}

// MarshalJSON writes the duration in its human-readable form.
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

// Check types.
const (
	TypeHTTP = "http"
	TypeTCP  = "tcp"
	TypeDNS  = "dns"
)

// Config is the root configuration document.
type Config struct {
	Listen      string   `json:"listen"`
	HistorySize int      `json:"history_size"`
	Targets     []Target `json:"targets"`
	Alerts      Alerts   `json:"alerts"`
}

// Target describes one endpoint to probe.
type Target struct {
	Name string `json:"name"`
	Type string `json:"type"`

	// HTTP
	URL                string            `json:"url,omitempty"`
	Method             string            `json:"method,omitempty"`
	Headers            map[string]string `json:"headers,omitempty"`
	ExpectStatus       []int             `json:"expect_status,omitempty"`
	ExpectBodyContains string            `json:"expect_body_contains,omitempty"`
	CertExpiryWarning  Duration          `json:"cert_expiry_warning,omitempty"`

	// TCP
	Address string `json:"address,omitempty"`

	// DNS
	Host string `json:"host,omitempty"`

	Interval          Duration `json:"interval,omitempty"`
	Timeout           Duration `json:"timeout,omitempty"`
	DegradedLatency   Duration `json:"degraded_latency,omitempty"`
	FailureThreshold  int      `json:"failure_threshold,omitempty"`
	RecoveryThreshold int      `json:"recovery_threshold,omitempty"`
	SLO               float64  `json:"slo,omitempty"` // percent, e.g. 99.9
}

// Endpoint returns the human-readable thing being probed.
func (t Target) Endpoint() string {
	switch t.Type {
	case TypeHTTP:
		return t.URL
	case TypeTCP:
		return t.Address
	case TypeDNS:
		return t.Host
	}
	return ""
}

// Alerts configures where state transitions are sent.
type Alerts struct {
	Webhooks []Webhook `json:"webhooks,omitempty"`
	// RepeatInterval re-sends a DOWN alert while a target stays down. 0 disables reminders.
	RepeatInterval Duration `json:"repeat_interval,omitempty"`
}

// Webhook formats.
const (
	FormatSlack   = "slack"
	FormatGeneric = "generic"
)

// Webhook is an HTTP endpoint that receives alert events.
type Webhook struct {
	Name   string `json:"name,omitempty"`
	URL    string `json:"url"`
	Format string `json:"format,omitempty"`
}

// Defaults applied when a field is left empty.
const (
	DefaultListen            = ":8080"
	DefaultHistorySize       = 1440 // 12h of 30s checks
	DefaultInterval          = 30 * time.Second
	DefaultTimeout           = 5 * time.Second
	DefaultFailureThreshold  = 3
	DefaultRecoveryThreshold = 2
	DefaultSLO               = 99.9
	DefaultCertExpiryWarning = 14 * 24 * time.Hour
	MinInterval              = 50 * time.Millisecond
)

// Load reads a config file, expands ${ENV_VARS} (so secrets such as webhook
// URLs stay out of the file), applies defaults and validates the result.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return Parse([]byte(os.ExpandEnv(string(raw))))
}

// Parse decodes, defaults and validates a config document.
func Parse(data []byte) (*Config, error) {
	var c Config
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = DefaultListen
	}
	if c.HistorySize == 0 {
		c.HistorySize = DefaultHistorySize
	}
	for i := range c.Targets {
		t := &c.Targets[i]
		t.Type = strings.ToLower(strings.TrimSpace(t.Type))
		if t.Interval == 0 {
			t.Interval = Duration(DefaultInterval)
		}
		if t.Timeout == 0 {
			t.Timeout = Duration(min(DefaultTimeout, t.Interval.D()))
		}
		if t.FailureThreshold == 0 {
			t.FailureThreshold = DefaultFailureThreshold
		}
		if t.RecoveryThreshold == 0 {
			t.RecoveryThreshold = DefaultRecoveryThreshold
		}
		if t.SLO == 0 {
			t.SLO = DefaultSLO
		}
		if t.Type == TypeHTTP {
			if t.Method == "" {
				t.Method = "GET"
			}
			t.Method = strings.ToUpper(t.Method)
			if t.CertExpiryWarning == 0 {
				t.CertExpiryWarning = Duration(DefaultCertExpiryWarning)
			}
		}
	}
	for i := range c.Alerts.Webhooks {
		w := &c.Alerts.Webhooks[i]
		if w.Format == "" {
			w.Format = FormatGeneric
		}
		if w.Name == "" {
			w.Name = fmt.Sprintf("webhook-%d", i+1)
		}
	}
}

// Validate reports every problem in the config at once rather than the first.
func (c *Config) Validate() error {
	var errs []error
	if c.HistorySize < 10 {
		errs = append(errs, fmt.Errorf("history_size must be at least 10"))
	}
	if len(c.Targets) == 0 {
		errs = append(errs, errors.New("at least one target is required"))
	}
	seen := map[string]bool{}
	for i, t := range c.Targets {
		prefix := fmt.Sprintf("targets[%d]", i)
		if t.Name != "" {
			prefix = fmt.Sprintf("target %q", t.Name)
		}
		fail := func(format string, a ...any) {
			errs = append(errs, fmt.Errorf(prefix+": "+format, a...))
		}
		if strings.TrimSpace(t.Name) == "" {
			fail("name is required")
		} else if seen[t.Name] {
			fail("duplicate name")
		}
		seen[t.Name] = true

		switch t.Type {
		case TypeHTTP:
			u, err := url.Parse(t.URL)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				fail("url must be an absolute http(s) URL, got %q", t.URL)
			}
			for _, s := range t.ExpectStatus {
				if s < 100 || s > 599 {
					fail("expect_status contains invalid code %d", s)
				}
			}
		case TypeTCP:
			if _, _, err := net.SplitHostPort(t.Address); err != nil {
				fail("address must be host:port, got %q", t.Address)
			}
		case TypeDNS:
			if strings.TrimSpace(t.Host) == "" {
				fail("host is required for dns checks")
			}
		default:
			fail("type must be one of http, tcp, dns; got %q", t.Type)
		}

		if t.Interval.D() < MinInterval {
			fail("interval must be at least %s", MinInterval)
		}
		if t.Timeout.D() <= 0 || t.Timeout.D() > t.Interval.D() {
			fail("timeout must be positive and no longer than interval")
		}
		if t.FailureThreshold < 1 || t.RecoveryThreshold < 1 {
			fail("failure_threshold and recovery_threshold must be >= 1")
		}
		if t.SLO <= 0 || t.SLO >= 100 {
			fail("slo must be between 0 and 100 (exclusive), got %v", t.SLO)
		}
		if t.DegradedLatency < 0 {
			fail("degraded_latency must not be negative")
		}
	}
	for i, w := range c.Alerts.Webhooks {
		u, err := url.Parse(w.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, fmt.Errorf("alerts.webhooks[%d]: url must be an absolute http(s) URL", i))
		}
		if w.Format != FormatSlack && w.Format != FormatGeneric {
			errs = append(errs, fmt.Errorf("alerts.webhooks[%d]: format must be slack or generic", i))
		}
	}
	if c.Alerts.RepeatInterval < 0 {
		errs = append(errs, errors.New("alerts.repeat_interval must not be negative"))
	}
	return errors.Join(errs...)
}
