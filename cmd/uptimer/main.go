// Command uptimer probes HTTP, TCP and DNS endpoints, tracks SLOs, serves a
// status page and Prometheus metrics, and sends alerts on state changes.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/rishi-vyas/uptimer/internal/alert"
	"github.com/rishi-vyas/uptimer/internal/config"
	"github.com/rishi-vyas/uptimer/internal/metrics"
	"github.com/rishi-vyas/uptimer/internal/monitor"
	"github.com/rishi-vyas/uptimer/internal/server"
	"github.com/rishi-vyas/uptimer/internal/store"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "uptimer:", err)
		os.Exit(1)
	}
}

func run() error {
	cfgPath := flag.String("config", "config.json", "path to config file")
	validate := flag.Bool("validate", false, "validate the config and exit")
	logLevel := flag.String("log-level", "info", "debug, info, warn or error")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return nil
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: parseLevel(*logLevel)}))

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if *validate {
		fmt.Printf("config OK: %d targets, %d webhooks\n", len(cfg.Targets), len(cfg.Alerts.Webhooks))
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	reg := metrics.NewUptimerRegistry(version)
	st := store.New(cfg.HistorySize)

	notifiers := []alert.Notifier{alert.LogNotifier{Logger: logger}}
	for _, w := range cfg.Alerts.Webhooks {
		notifiers = append(notifiers, alert.NewWebhook(w))
	}
	dispatcher := alert.NewDispatcher(logger, 256, notifiers...)

	mon, err := monitor.New(monitor.Options{
		Targets:        cfg.Targets,
		Store:          st,
		Metrics:        reg,
		Alerter:        dispatcher,
		Logger:         logger,
		RepeatInterval: cfg.Alerts.RepeatInterval.D(),
		StatusURL:      os.Getenv("UPTIMER_PUBLIC_URL"),
	})
	if err != nil {
		return err
	}

	srv := &server.Server{
		Monitor: mon, Store: st, Metrics: reg, Logger: logger,
		BeforeScrape: func() {
			dropped, _ := dispatcher.Counts()
			reg.Set(metrics.AlertsDropped, float64(dropped))
		},
	}
	httpServer := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// Alerts get their own context so they can drain after probes stop.
	alertCtx, stopAlerts := context.WithCancel(context.Background())
	alertsDone := make(chan struct{})
	monDone := make(chan struct{})
	go func() { defer close(alertsDone); dispatcher.Run(alertCtx) }()
	go func() { defer close(monDone); mon.Run(ctx) }()
	// Stop order: probes first, then drain alerts, so a final transition
	// observed during shutdown is still delivered.
	waitAll := func() { <-monDone; stopAlerts(); <-alertsDone }

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("uptimer started", "version", version, "listen", cfg.Listen, "targets", len(cfg.Targets))
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
		close(serveErr)
	}()

	select {
	case <-ctx.Done():
		logger.Info("shutting down")
	case err := <-serveErr:
		stop()
		waitAll()
		return fmt.Errorf("http server: %w", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("http shutdown", "err", err)
	}
	waitAll()
	logger.Info("stopped cleanly")
	return nil
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	}
	return slog.LevelInfo
}
