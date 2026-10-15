// Package check implements the probes uptimer runs against targets.
package check

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/rishi-vyas/uptimer/internal/config"
)

// Result is the outcome of a single probe.
type Result struct {
	Target     string        `json:"target"`
	CheckedAt  time.Time     `json:"checked_at"`
	OK         bool          `json:"ok"`
	Degraded   bool          `json:"degraded"`
	Latency    time.Duration `json:"latency_ns"`
	StatusCode int           `json:"status_code,omitempty"`
	Reason     string        `json:"reason,omitempty"`
	CertExpiry time.Time     `json:"cert_expiry,omitempty"`
}

// Checker probes one target.
type Checker interface {
	Check(ctx context.Context) Result
}

// New builds the right checker for a target.
func New(t config.Target) (Checker, error) {
	switch t.Type {
	case config.TypeHTTP:
		return newHTTP(t), nil
	case config.TypeTCP:
		return &tcpChecker{target: t}, nil
	case config.TypeDNS:
		return &dnsChecker{target: t, resolver: net.DefaultResolver}, nil
	}
	return nil, fmt.Errorf("unknown check type %q", t.Type)
}

// maxBody bounds how much of a response we read when matching keywords.
const maxBody = 1 << 20

type httpChecker struct {
	target config.Target
	client *http.Client
}

func newHTTP(t config.Target) *httpChecker {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// A fresh connection per probe means latency includes DNS + TCP + TLS,
	// which is what a real first-time user experiences.
	transport.DisableKeepAlives = true
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	return &httpChecker{
		target: t,
		client: &http.Client{Transport: transport, Timeout: t.Timeout.D()},
	}
}

func (c *httpChecker) Check(ctx context.Context) Result {
	t := c.target
	res := Result{Target: t.Name, CheckedAt: time.Now()}

	req, err := http.NewRequestWithContext(ctx, t.Method, t.URL, nil)
	if err != nil {
		res.Reason = "build request: " + err.Error()
		return res
	}
	req.Header.Set("User-Agent", "uptimer/1.0")
	for k, v := range t.Headers {
		req.Header.Set(k, v)
	}

	start := time.Now()
	resp, err := c.client.Do(req)
	if err != nil {
		res.Latency = time.Since(start)
		res.Reason = classifyNetErr(err)
		return res
	}
	defer resp.Body.Close()

	var body []byte
	if t.ExpectBodyContains != "" {
		body, err = io.ReadAll(io.LimitReader(resp.Body, maxBody))
	} else {
		_, err = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
	}
	res.Latency = time.Since(start)
	res.StatusCode = resp.StatusCode
	if err != nil {
		res.Reason = "read body: " + classifyNetErr(err)
		return res
	}

	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		res.CertExpiry = resp.TLS.PeerCertificates[0].NotAfter
	}

	if !statusAccepted(resp.StatusCode, t.ExpectStatus) {
		res.Reason = fmt.Sprintf("unexpected status %d", resp.StatusCode)
		return res
	}
	if t.ExpectBodyContains != "" && !strings.Contains(string(body), t.ExpectBodyContains) {
		res.Reason = fmt.Sprintf("body does not contain %q", t.ExpectBodyContains)
		return res
	}

	res.OK = true
	markDegraded(&res, t)
	if !res.CertExpiry.IsZero() && t.CertExpiryWarning > 0 {
		left := time.Until(res.CertExpiry)
		if left < t.CertExpiryWarning.D() {
			res.Degraded = true
			res.Reason = joinReason(res.Reason, fmt.Sprintf("certificate expires in %s", left.Round(time.Hour)))
		}
	}
	return res
}

func statusAccepted(code int, expect []int) bool {
	if len(expect) == 0 {
		return code >= 200 && code < 400
	}
	return slices.Contains(expect, code)
}

type tcpChecker struct{ target config.Target }

func (c *tcpChecker) Check(ctx context.Context) Result {
	t := c.target
	res := Result{Target: t.Name, CheckedAt: time.Now()}
	ctx, cancel := context.WithTimeout(ctx, t.Timeout.D())
	defer cancel()

	start := time.Now()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", t.Address)
	res.Latency = time.Since(start)
	if err != nil {
		res.Reason = classifyNetErr(err)
		return res
	}
	_ = conn.Close()
	res.OK = true
	markDegraded(&res, t)
	return res
}

type resolver interface {
	LookupHost(ctx context.Context, host string) ([]string, error)
}

type dnsChecker struct {
	target   config.Target
	resolver resolver
}

func (c *dnsChecker) Check(ctx context.Context) Result {
	t := c.target
	res := Result{Target: t.Name, CheckedAt: time.Now()}
	ctx, cancel := context.WithTimeout(ctx, t.Timeout.D())
	defer cancel()

	start := time.Now()
	addrs, err := c.resolver.LookupHost(ctx, t.Host)
	res.Latency = time.Since(start)
	if err != nil {
		res.Reason = classifyNetErr(err)
		return res
	}
	if len(addrs) == 0 {
		res.Reason = "no addresses returned"
		return res
	}
	res.OK = true
	markDegraded(&res, t)
	return res
}

func markDegraded(res *Result, t config.Target) {
	if t.DegradedLatency > 0 && res.Latency > t.DegradedLatency.D() {
		res.Degraded = true
		res.Reason = joinReason(res.Reason, fmt.Sprintf("slow response: %s > %s",
			res.Latency.Round(time.Millisecond), t.DegradedLatency.D()))
	}
}

func joinReason(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}

// classifyNetErr turns low-level errors into short, alert-friendly reasons.
func classifyNetErr(err error) string {
	var dnsErr *net.DNSError
	var opErr *net.OpError
	var certErr *tls.CertificateVerificationError
	switch {
	case errors.Is(err, context.DeadlineExceeded) || isTimeout(err):
		return "timeout"
	case errors.As(err, &dnsErr):
		if dnsErr.IsNotFound {
			return "dns: no such host"
		}
		return "dns: " + dnsErr.Err
	case errors.As(err, &certErr):
		return "tls: " + certErr.Err.Error()
	case errors.As(err, &opErr) && opErr.Op == "dial":
		return "connection failed: " + opErr.Err.Error()
	}
	return err.Error()
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}
