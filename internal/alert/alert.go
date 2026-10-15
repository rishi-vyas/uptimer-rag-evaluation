// Package alert delivers state-transition events to notifiers (logs, webhooks)
// without ever blocking the probing loop.
package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/rishi-vyas/uptimer/internal/config"
)

// Event describes a status change (or a reminder that a target is still down).
type Event struct {
	Target    string        `json:"target"`
	Endpoint  string        `json:"endpoint"`
	From      string        `json:"from"`
	To        string        `json:"to"`
	At        time.Time     `json:"at"`
	Reason    string        `json:"reason,omitempty"`
	Latency   time.Duration `json:"latency_ns"`
	Duration  time.Duration `json:"previous_state_duration_ns"`
	Reminder  bool          `json:"reminder"`
	StatusURL string        `json:"status_url,omitempty"`
}

// Summary is a one-line, human-readable rendering of the event.
func (e Event) Summary() string {
	icon := map[string]string{"down": "🔴", "up": "🟢", "degraded": "🟡"}[e.To]
	switch {
	case e.Reminder:
		return fmt.Sprintf("%s %s is still DOWN (%s) — %s", icon, e.Target, e.Endpoint, e.Reason)
	case e.To == "up" && e.From == "down":
		return fmt.Sprintf("%s %s RECOVERED after %s (%s)", icon, e.Target, e.Duration.Round(time.Second), e.Endpoint)
	case e.Reason != "":
		return fmt.Sprintf("%s %s is %s (%s) — %s", icon, e.Target, upper(e.To), e.Endpoint, e.Reason)
	}
	return fmt.Sprintf("%s %s is %s (%s)", icon, e.Target, upper(e.To), e.Endpoint)
}

func upper(s string) string { return string(bytes.ToUpper([]byte(s))) }

// Notifier sends an event somewhere.
type Notifier interface {
	Name() string
	Notify(ctx context.Context, e Event) error
}

// LogNotifier writes events to structured logs. It is always enabled.
type LogNotifier struct{ Logger *slog.Logger }

func (LogNotifier) Name() string { return "log" }

func (n LogNotifier) Notify(_ context.Context, e Event) error {
	n.Logger.Warn("alert", "target", e.Target, "from", e.From, "to", e.To,
		"reason", e.Reason, "reminder", e.Reminder, "summary", e.Summary())
	return nil
}

// Webhook posts events as JSON, retrying transient failures with exponential backoff.
type Webhook struct {
	cfg     config.Webhook
	client  *http.Client
	retries int
	backoff time.Duration
}

// NewWebhook builds a webhook notifier.
func NewWebhook(cfg config.Webhook) *Webhook {
	return &Webhook{cfg: cfg, client: &http.Client{Timeout: 10 * time.Second}, retries: 3, backoff: time.Second}
}

func (w *Webhook) Name() string { return w.cfg.Name }

// errPermanent marks failures that retrying will not fix (4xx, bad payload).
var errPermanent = errors.New("permanent")

func (w *Webhook) Notify(ctx context.Context, e Event) error {
	payload, err := w.payload(e)
	if err != nil {
		return err
	}
	var lastErr error
	for attempt := 0; attempt <= w.retries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(w.backoff << (attempt - 1)):
			}
		}
		lastErr = w.post(ctx, payload)
		if lastErr == nil || errors.Is(lastErr, errPermanent) {
			return lastErr
		}
	}
	return fmt.Errorf("after %d attempts: %w", w.retries+1, lastErr)
}

func (w *Webhook) payload(e Event) ([]byte, error) {
	if w.cfg.Format == config.FormatSlack {
		return json.Marshal(map[string]string{"text": e.Summary()})
	}
	return json.Marshal(e)
}

func (w *Webhook) post(ctx context.Context, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%w: %v", errPermanent, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	switch {
	case resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return fmt.Errorf("webhook returned %d", resp.StatusCode)
	default:
		return fmt.Errorf("%w: webhook returned %d", errPermanent, resp.StatusCode)
	}
}

// Dispatcher fans events out to notifiers from a bounded queue so a slow or
// broken webhook can never stall the probes. If the queue fills up, new events
// are dropped and counted rather than blocking.
type Dispatcher struct {
	notifiers []Notifier
	queue     chan Event
	logger    *slog.Logger
	wg        sync.WaitGroup

	mu      sync.Mutex
	dropped int
	failed  int
}

// NewDispatcher creates a dispatcher with the given queue size.
func NewDispatcher(logger *slog.Logger, queueSize int, notifiers ...Notifier) *Dispatcher {
	return &Dispatcher{notifiers: notifiers, queue: make(chan Event, max(1, queueSize)), logger: logger}
}

// Send enqueues an event without blocking.
func (d *Dispatcher) Send(e Event) {
	select {
	case d.queue <- e:
	default:
		d.mu.Lock()
		d.dropped++
		d.mu.Unlock()
		d.logger.Error("alert queue full, dropping event", "target", e.Target, "to", e.To)
	}
}

// Run delivers events until ctx is cancelled, then drains what is queued
// (bounded by a short grace period) so shutdown does not lose a final alert.
func (d *Dispatcher) Run(ctx context.Context) {
	for {
		select {
		case e := <-d.queue:
			d.deliver(ctx, e)
		case <-ctx.Done():
			drainCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			for {
				select {
				case e := <-d.queue:
					d.deliver(drainCtx, e)
				default:
					d.wg.Wait()
					return
				}
			}
		}
	}
}

func (d *Dispatcher) deliver(ctx context.Context, e Event) {
	for _, n := range d.notifiers {
		d.wg.Add(1)
		go func(n Notifier) {
			defer d.wg.Done()
			if err := n.Notify(ctx, e); err != nil {
				d.mu.Lock()
				d.failed++
				d.mu.Unlock()
				d.logger.Error("alert delivery failed", "notifier", n.Name(), "target", e.Target, "err", err)
			}
		}(n)
	}
}

// Counts returns how many events were dropped and how many deliveries failed.
func (d *Dispatcher) Counts() (dropped, failed int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dropped, d.failed
}
