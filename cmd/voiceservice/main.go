package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"voice-timeline-renderer/internal/httpapi"
	"voice-timeline-renderer/internal/service"
	"voice-timeline-renderer/internal/storage"
)

func main() {
	address := flag.String("addr", ":8093", "HTTP listen address")
	dataDir := flag.String("data", "./var/voice-timeline", "persistent data directory")
	retention := flag.Duration("render-retention", 7*24*time.Hour, "how long a completed render output stays downloadable before reclamation")
	sweepInterval := flag.Duration("render-retention-sweep", time.Minute, "interval between render retention sweeps (0 disables the background sweeper)")
	flag.Parse()

	if err := os.MkdirAll(*dataDir, 0o750); err != nil {
		slog.Error("cannot create data directory", "error", err)
		os.Exit(1)
	}
	repo, err := storage.NewRepository(*dataDir)
	if err != nil {
		slog.Error("cannot initialize repository", "error", err)
		os.Exit(1)
	}
	serviceLayer := service.New(repo, service.Config{RenderRetention: *retention})
	api := httpapi.New(serviceLayer, httpapi.Options{
		MaxSourceRequestBytes: 48 << 20,
		MaxJSONBytes:          2 << 20,
	})
	httpServer := &http.Server{
		Addr:              *address,
		Handler:           api.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      180 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	runContext, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-runContext.Done()
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownContext)
	}()

	if *sweepInterval > 0 {
		go runRetentionSweeps(runContext, serviceLayer, *sweepInterval)
	}

	slog.Info("voice timeline service listening",
		"addr", *address,
		"data", *dataDir,
		"render_retention", retention.String(),
		"render_retention_sweep", sweepInterval.String(),
	)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("HTTP service stopped", "error", err)
		os.Exit(1)
	}
}

// runRetentionSweeps reconciles render artifacts once at startup (healing any
// crash window or manual deletion from while the process was down) and then
// reclaims expired outputs on the configured interval.
func runRetentionSweeps(ctx context.Context, serviceLayer *service.Service, interval time.Duration) {
	sweep := func() {
		summary, err := serviceLayer.ReclaimExpiredRenders(ctx, time.Now().UTC())
		if err != nil {
			slog.Warn("render retention sweep finished with errors", "error", err)
		}
		if summary.Total() > 0 {
			slog.Info("render retention sweep reclaimed artifacts",
				"expired", summary.Expired,
				"missing", summary.Missing,
				"finalized", summary.Finalized,
				"backfilled", summary.Backfilled,
			)
		}
	}
	sweep()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweep()
		}
	}
}
