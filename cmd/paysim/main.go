// Command paysim runs the deliberately unreliable fake payment provider
// (docs/decisions/023).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/AryanSingh103/auction-engine/internal/config"
	"github.com/AryanSingh103/auction-engine/internal/paysim"
	"github.com/AryanSingh103/auction-engine/internal/postgres"
)

const (
	dbMaxConns = 10
	// A simulator, not a service under test: fixed timeouts are enough.
	// The write timeout must outlast an injected hang, or the hang would
	// turn into a dropped connection instead of a slow success.
	readHeaderTimeout = 5 * time.Second
	writeSlack        = 10 * time.Second
	shutdownTimeout   = 5 * time.Second
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "paysim: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.LoadPaysim(os.LookupEnv)
	if err != nil {
		return fmt.Errorf("load config:\n%w", err)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// The simulator does not wait on a transaction across requests, so the
	// idle-in-transaction timeout only needs to be a safety net.
	pool, err := postgres.NewPool(ctx, postgres.PoolConfig{URL: cfg.DatabaseURL, MaxConns: dbMaxConns, IdleInTxTimeout: time.Minute})
	if err != nil {
		return err
	}
	defer pool.Close()
	store := paysim.NewStore(pool)
	if err := store.Migrate(ctx); err != nil {
		return err
	}

	faults := paysim.Faults{FailureRate: cfg.FailureRate, HangRate: cfg.HangRate, Hang: cfg.HangDuration, Roll: rand.Float64}
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           paysim.NewHandler(store, faults, logger),
		ReadHeaderTimeout: readHeaderTimeout,
		WriteTimeout:      cfg.HangDuration + writeSlack,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ListenAndServe() }()
	logger.Info("paysim started", slog.String("addr", cfg.Addr),
		slog.Float64("failure_rate", cfg.FailureRate), slog.Float64("hang_rate", cfg.HangRate))

	select {
	case err := <-serveErr:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}
