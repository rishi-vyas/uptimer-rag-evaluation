package check

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rishi-vyas/uptimer/internal/config"
)

func httpTarget(url string, mod func(*config.Target)) config.Target {
	t := config.Target{
		Name: "t", Type: config.TypeHTTP, URL: url, Method: "GET",
		Interval: config.Duration(time.Second), Timeout: config.Duration(time.Second),
	}
	if mod != nil {
		mod(&t)
	}
	return t
}

func run(t *testing.T, tg config.Target) Result {
	t.Helper()
	c, err := New(tg)
	if err != nil {
		t.Fatal(err)
	}
	return c.Check(context.Background())
}

func TestHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			if r.Header.Get("X-Token") != "s3cret" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"status":"healthy"}`))
		case "/500":
			w.WriteHeader(http.StatusInternalServerError)
		case "/slow":
			time.Sleep(80 * time.Millisecond)
		case "/hang":
			time.Sleep(2 * time.Second)
		}
	}))
	defer srv.Close()
	auth := func(t *config.Target) { t.Headers = map[string]string{"X-Token": "s3cret"} }

	tests := []struct {
		name       string
		target     config.Target
		ok, degr   bool
		reasonPart string
	}{
		{"healthy", httpTarget(srv.URL+"/ok", auth), true, false, ""},
		{"keyword present", httpTarget(srv.URL+"/ok", func(t *config.Target) { auth(t); t.ExpectBodyContains = `"healthy"` }), true, false, ""},
		{"keyword missing", httpTarget(srv.URL+"/ok", func(t *config.Target) { auth(t); t.ExpectBodyContains = "ready" }), false, false, "body does not contain"},
		{"server error", httpTarget(srv.URL+"/500", nil), false, false, "unexpected status 500"},
		{"explicit expected status", httpTarget(srv.URL+"/500", func(t *config.Target) { t.ExpectStatus = []int{500} }), true, false, ""},
		{"missing header -> 401", httpTarget(srv.URL+"/ok", nil), false, false, "unexpected status 401"},
		{"slow -> degraded", httpTarget(srv.URL+"/slow", func(t *config.Target) { t.DegradedLatency = config.Duration(20 * time.Millisecond) }), true, true, "slow response"},
		{"timeout", httpTarget(srv.URL+"/hang", func(t *config.Target) { t.Timeout = config.Duration(100 * time.Millisecond) }), false, false, "timeout"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := run(t, tc.target)
			if r.OK != tc.ok || r.Degraded != tc.degr {
				t.Fatalf("ok=%v degraded=%v reason=%q; want ok=%v degraded=%v", r.OK, r.Degraded, r.Reason, tc.ok, tc.degr)
			}
			if tc.reasonPart != "" && !strings.Contains(r.Reason, tc.reasonPart) {
				t.Errorf("reason %q does not contain %q", r.Reason, tc.reasonPart)
			}
			if r.Latency <= 0 || r.CheckedAt.IsZero() || r.Target != "t" {
				t.Errorf("metadata not filled: %+v", r)
			}
		})
	}
}

func TestHTTPSCertExpiry(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	c := newHTTP(httpTarget(srv.URL, func(t *config.Target) {
		// httptest's cert expires in 2084; warn window larger than that => degraded.
		t.CertExpiryWarning = config.Duration(100 * 365 * 24 * time.Hour)
	}))
	c.client.Transport.(*http.Transport).TLSClientConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig
	r := c.Check(context.Background())
	if !r.OK || r.CertExpiry.IsZero() {
		t.Fatalf("expected ok with cert expiry, got %+v", r)
	}
	if !r.Degraded || !strings.Contains(r.Reason, "certificate expires") {
		t.Errorf("expected cert-expiry degradation, got %+v", r)
	}
}

func TestHTTPSUntrustedCertFails(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	r := run(t, httpTarget(srv.URL, nil))
	if r.OK || !strings.HasPrefix(r.Reason, "tls:") {
		t.Errorf("expected tls failure, got %+v", r)
	}
}

func TestTCP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	addr := ln.Addr().String()
	tg := config.Target{Name: "t", Type: config.TypeTCP, Address: addr, Timeout: config.Duration(time.Second)}
	if r := run(t, tg); !r.OK {
		t.Fatalf("expected open port to pass: %+v", r)
	}
	ln.Close()
	r := run(t, tg)
	if r.OK || !strings.Contains(r.Reason, "connection failed") {
		t.Errorf("expected closed port to fail with connection error: %+v", r)
	}
}

type fakeResolver struct {
	addrs []string
	err   error
}

func (f fakeResolver) LookupHost(context.Context, string) ([]string, error) { return f.addrs, f.err }

func TestDNS(t *testing.T) {
	tg := config.Target{Name: "t", Type: config.TypeDNS, Host: "example.com", Timeout: config.Duration(time.Second)}
	cases := []struct {
		name   string
		res    fakeResolver
		ok     bool
		reason string
	}{
		{"resolves", fakeResolver{addrs: []string{"93.184.216.34"}}, true, ""},
		{"nxdomain", fakeResolver{err: &net.DNSError{Err: "no such host", IsNotFound: true}}, false, "dns: no such host"},
		{"empty", fakeResolver{}, false, "no addresses"},
		{"timeout", fakeResolver{err: context.DeadlineExceeded}, false, "timeout"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := (&dnsChecker{target: tg, resolver: tc.res}).Check(context.Background())
			if r.OK != tc.ok || !strings.Contains(r.Reason, tc.reason) {
				t.Errorf("got ok=%v reason=%q", r.OK, r.Reason)
			}
		})
	}
}

func TestDNSRealLocalhost(t *testing.T) {
	r := run(t, config.Target{Name: "t", Type: config.TypeDNS, Host: "localhost", Timeout: config.Duration(2 * time.Second)})
	if !r.OK {
		t.Skipf("localhost did not resolve in this environment: %s", r.Reason)
	}
}

func TestClassifyNetErr(t *testing.T) {
	if got := classifyNetErr(errors.New("boom")); got != "boom" {
		t.Errorf("passthrough: %q", got)
	}
	if got := classifyNetErr(context.DeadlineExceeded); got != "timeout" {
		t.Errorf("deadline: %q", got)
	}
}

func TestUnknownType(t *testing.T) {
	if _, err := New(config.Target{Type: "smtp"}); err == nil {
		t.Error("expected error")
	}
}
