// Command api is the auction-engine HTTP API process.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/AryanSingh103/auction-engine/internal/config"
	"github.com/AryanSingh103/auction-engine/internal/httpapi"
)

// main only translates run's error into an exit code. Keeping os.Exit out of
// run means every deferred cleanup in run actually executes; os.Exit skips
// deferred calls.
func main() {
	if err := run(); err != nil {
		// Plain stderr, not slog: if config failed to load, no logger exists.
		fmt.Fprintf(os.Stderr, "api: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(os.LookupEnv)
	if err != nil {
		return fmt.Errorf("load config:\n%w", err)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))

	// ctx is cancelled on the first SIGINT (Ctrl-C) or SIGTERM (docker stop,
	// ECS task stop).
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{
		Handler:           httpapi.NewRouter(logger),
		ReadHeaderTimeout: cfg.HTTPReadHeaderTimeout,
		ReadTimeout:       cfg.HTTPReadTimeout,
		WriteTimeout:      cfg.HTTPWriteTimeout,
		IdleTimeout:       cfg.HTTPIdleTimeout,
		// net/http reports connection-level problems (TLS handshake errors,
		// panics it recovered itself) through a *log.Logger; route them into
		// the same JSON stream.
		ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}

	// Bind before starting the serve goroutine so "address already in use"
	// is returned here, synchronously, instead of racing with the shutdown
	// select below.
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("listen on %q: %w", cfg.HTTPAddr, err)
	}

	// Buffered so the goroutine can always deliver its result and exit, even
	// if run has already returned on another path.
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.Serve(ln)
	}()
	logger.Info("server started", slog.String("addr", ln.Addr().String()))

	select {
	case err := <-serveErr:
		// Serve always returns a non-nil error. ErrServerClosed only follows
		// Shutdown or Close, neither of which has been called yet, so any
		// error here is a real failure.
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}

	// Restore default signal handling: a second Ctrl-C now kills the process
	// immediately instead of being swallowed while we drain.
	stop()
	logger.Info("shutdown started", slog.Duration("timeout", cfg.ShutdownTimeout))

	// A fresh context: ctx is already cancelled, and deriving from it would
	// give Shutdown zero time.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()

	// Shutdown closes the listener, closes idle connections, and waits for
	// active requests to finish, or for shutdownCtx to expire.
	if err := srv.Shutdown(shutdownCtx); err != nil {
		// Drain timed out: force-close whatever is still open so the process
		// can exit, and report failure.
		closeErr := srv.Close()
		return errors.Join(fmt.Errorf("graceful shutdown: %w", err), closeErr)
	}

	// Serve has returned ErrServerClosed by now; anything else is a failure
	// that happened while we were shutting down.
	if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}

	logger.Info("shutdown complete")
	return nil
}
