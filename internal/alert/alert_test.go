package alert

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rishi-vyas/uptimer/internal/config"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func fastWebhook(url, format string) *Webhook {
	w := NewWebhook(config.Webhook{Name: "test", URL: url, Format: format})
	w.backoff = time.Millisecond
	return w
}

func TestWebhookRetriesTransientFailures(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	if err := fastWebhook(srv.URL, config.FormatGeneric).Notify(context.Background(), Event{Target: "api"}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 {
		t.Errorf("calls = %d, want 3", calls.Load())
	}
}

func TestWebhookDoesNotRetryClientErrors(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()
	err := fastWebhook(srv.URL, config.FormatGeneric).Notify(context.Background(), Event{})
	if err == nil || calls.Load() != 1 {
		t.Errorf("err=%v calls=%d; want error after 1 call", err, calls.Load())
	}
}

func TestWebhookGivesUpAfterRetries(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	err := fastWebhook(srv.URL, config.FormatGeneric).Notify(context.Background(), Event{})
	if err == nil || calls.Load() != 4 {
		t.Errorf("err=%v calls=%d; want error after 4 calls", err, calls.Load())
	}
}

func TestPayloadFormats(t *testing.T) {
	var got []map[string]any
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		mu.Lock()
		got = append(got, m)
		mu.Unlock()
	}))
	defer srv.Close()
	e := Event{Target: "api", Endpoint: "https://x/health", From: "up", To: "down", Reason: "timeout"}
	_ = fastWebhook(srv.URL, config.FormatSlack).Notify(context.Background(), e)
	_ = fastWebhook(srv.URL, config.FormatGeneric).Notify(context.Background(), e)

	if text, _ := got[0]["text"].(string); !strings.Contains(text, "api is DOWN") || !strings.Contains(text, "timeout") {
		t.Errorf("slack text = %q", text)
	}
	if got[1]["target"] != "api" || got[1]["to"] != "down" {
		t.Errorf("generic payload = %v", got[1])
	}
}

func TestSummary(t *testing.T) {
	cases := map[string]Event{
		"api RECOVERED after 2m0s": {Target: "api", From: "down", To: "up", Duration: 2 * time.Minute},
		"api is still DOWN":        {Target: "api", From: "down", To: "down", Reminder: true},
		"api is DEGRADED":          {Target: "api", From: "up", To: "degraded", Reason: "slow"},
	}
	for want, e := range cases {
		if !strings.Contains(e.Summary(), want) {
			t.Errorf("Summary() = %q, want it to contain %q", e.Summary(), want)
		}
	}
}

type recorder struct {
	mu     sync.Mutex
	events []Event
	block  chan struct{}
}

func (r *recorder) Name() string { return "rec" }
func (r *recorder) Notify(ctx context.Context, e Event) error {
	if r.block != nil {
		<-r.block
	}
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
	return nil
}
func (r *recorder) count() int { r.mu.Lock(); defer r.mu.Unlock(); return len(r.events) }

func TestDispatcherDrainsOnShutdown(t *testing.T) {
	rec := &recorder{}
	d := NewDispatcher(quiet, 10, rec)
	for i := 0; i < 5; i++ {
		d.Send(Event{Target: "x"})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled: Run must still drain the queue
	d.Run(ctx)
	if rec.count() != 5 {
		t.Errorf("delivered %d, want 5", rec.count())
	}
}

func TestDispatcherDropsWhenFullInsteadOfBlocking(t *testing.T) {
	d := NewDispatcher(quiet, 2, &recorder{})
	done := make(chan struct{})
	go func() {
		for i := 0; i < 10; i++ {
			d.Send(Event{})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Send blocked on a full queue")
	}
	if dropped, _ := d.Counts(); dropped != 8 {
		t.Errorf("dropped = %d, want 8", dropped)
	}
}
