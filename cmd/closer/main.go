// Command closer closes auctions once they end (docs/decisions/027). Run
// two or more: one leads, chosen by a Postgres advisory lock, and a standby
// takes over within about CLOSER_INTERVAL of the leader's session ending.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/AryanSingh103/auction-engine/internal/auction"
	"github.com/AryanSingh103/auction-engine/internal/closer"
	"github.com/AryanSingh103/auction-engine/internal/config"
	"github.com/AryanSingh103/auction-engine/internal/metrics"
	"github.com/AryanSingh103/auction-engine/internal/postgres"
)

// One connection holds the leader lock (taken out of the pool for good);
// the others run the scan and the close transactions, one at a time.
const dbMaxConns = 3

// Bounds a metrics scrape and the metrics server's shutdown.
const metricsTimeout = 5 * time.Second

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "closer: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.LoadCloser(os.LookupEnv)
	if err != nil {
		return fmt.Errorf("load config:\n%w", err)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))

	// Cancelled on SIGINT or SIGTERM. A close in flight then rolls back;
	// the next leader closes that auction instead.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := postgres.NewPool(ctx, postgres.PoolConfig{
		URL:             cfg.DatabaseURL,
		MaxConns:        dbMaxConns,
		IdleInTxTimeout: cfg.DBIdleInTxTimeout,
	})
	if err != nil {
		return err
	}
	defer pool.Close()

	reg := metrics.NewWorkerRegistry()
	closerMetrics := metrics.NewCloser(reg)
	stopMetrics, err := metrics.StartServer(ctx, cfg.MetricsAddr, reg.Handler(), metricsTimeout)
	if err != nil {
		return err
	}
	defer stopMetrics()

	// No cache or publisher: a reader may see a closed auction as open
	// until AUCTION_CACHE_TTL, but bids are refused from end_at on either
	// way (ADR 027).
	svc := auction.NewService(pool)
	logger.Info("closer started",
		slog.Duration("interval", cfg.CloserInterval),
		slog.Int("batch_size", int(cfg.CloserBatchSize)))
	closer.New(pool, svc, cfg.CloserInterval, int(cfg.CloserBatchSize), closerMetrics, logger).Run(ctx)
	logger.Info("closer stopped")
	return nil
}
