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

	"github.com/AryanSingh103/auction-engine/internal/auction"
	"github.com/AryanSingh103/auction-engine/internal/cache"
	"github.com/AryanSingh103/auction-engine/internal/config"
	"github.com/AryanSingh103/auction-engine/internal/httpapi"
	"github.com/AryanSingh103/auction-engine/internal/live"
	"github.com/AryanSingh103/auction-engine/internal/metrics"
	"github.com/AryanSingh103/auction-engine/internal/postgres"
	"github.com/AryanSingh103/auction-engine/internal/ratelimit"
	"github.com/AryanSingh103/auction-engine/internal/redisclient"
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

	// The pool connects lazily, so the API starts even if Postgres is not
	// reachable yet; /readyz reports that until it is. A malformed
	// DATABASE_URL, though, is a config error and fails startup.
	pool, err := postgres.NewPool(context.Background(), postgres.PoolConfig{
		URL:             cfg.DatabaseURL,
		MaxConns:        cfg.DBMaxConns,
		IdleInTxTimeout: cfg.DBIdleInTxTimeout,
	})
	if err != nil {
		return err
	}
	// Closed after the HTTP server has drained (see the end of run), so no
	// in-flight request loses its connection mid-transaction. The defer
	// also covers early error returns.
	defer pool.Close()

	// ctx is cancelled on the first SIGINT (Ctrl-C) or SIGTERM (docker stop,
	// ECS task stop).
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Root of every request context. Shutdown never cancels request
	// contexts on its own; cancelling this one when the drain deadline
	// passes tells in-flight handlers (DB calls from M1 on) to stop, instead
	// of leaving them running after their connections are force-closed.
	requestCtx, cancelRequests := context.WithCancel(context.Background())
	defer cancelRequests()

	rdb, err := redisclient.New(cfg.RedisURL, cfg.RedisTimeout)
	if err != nil {
		return err
	}
	// Closed after the pool (end of run); the defer covers early returns.
	defer func() { _ = rdb.Close() }()

	m := metrics.New()
	m.RegisterPool(pool)

	// Live updates: this instance's hub of WebSocket rooms, fed by Redis
	// pub/sub so bids accepted on any instance reach every client.
	hub := live.NewHub(int(cfg.WSSendBuffer), m.LiveDropped)
	bus := live.NewBus(rdb, m.LiveBusMessage)
	liveCtx, stopLive := context.WithCancel(context.Background())
	defer stopLive()
	// Forward retries until Redis is reachable, so an instance started
	// during a Redis outage still gets live updates afterwards.
	_, busDone := bus.Forward(liveCtx, hub)

	svc := auction.NewService(pool,
		auction.WithObserver(m),
		auction.WithLocking(auction.Locking(cfg.BidLocking)),
		auction.WithCache(cache.New(rdb, cfg.AuctionCacheTTL, m.CacheOperation)),
		auction.WithPublisher(bus))
	// Running WebSocket handlers, awaited at shutdown (see below).
	var liveHandlers httpapi.ActiveHandlers
	syncDone := httpapi.RunLiveSync(liveCtx, svc, hub, cfg.WSSyncInterval, logger)

	srv := &http.Server{
		Handler: httpapi.NewRouter(httpapi.Options{
			Logger:         logger,
			Auctions:       svc,
			Ready:          pool.Ping,
			RequestTimeout: cfg.RequestTimeout,
			Metrics:        m.HTTPMiddleware,
			BidLimiter: ratelimit.New(rdb, "ratelimit:",
				cfg.RateLimitBidsPerSecond, int(cfg.RateLimitBidBurst)),
			RecordRateLimit: m.RateLimitDecision,
			MaxInFlightBids: int(cfg.BidMaxInFlight),
			RecordShed:      m.BidShedding,
			Live: &httpapi.LiveOptions{
				Hub: hub, PingInterval: cfg.WSPingInterval, WriteTimeout: cfg.WSWriteTimeout,
				OnConnections: m.LiveConnections,
				Slots:         make(chan struct{}, cfg.WSMaxConnections),
				Active:        &liveHandlers,
			},
		}),
		BaseContext:       func(net.Listener) context.Context { return requestCtx },
		ReadHeaderTimeout: cfg.HTTPReadHeaderTimeout,
		ReadTimeout:       cfg.HTTPReadTimeout,
		WriteTimeout:      cfg.HTTPWriteTimeout,
		IdleTimeout:       cfg.HTTPIdleTimeout,
		// net/http reports connection-level problems (TLS handshake errors,
		// panics it recovered itself) through a *log.Logger; route them into
		// the same JSON stream.
		ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}

	// Shutdown does not track hijacked (WebSocket) connections: close them
	// explicitly with 1001 Going Away so clients reconnect elsewhere.
	srv.RegisterOnShutdown(hub.Close)

	// The metrics server is separate so /metrics is never reachable on the
	// public port (docs/decisions/013). It has the same timeouts.
	metricsMux := http.NewServeMux()
	metricsMux.Handle("GET /metrics", m.Handler())
	metricsSrv := &http.Server{
		Handler:           metricsMux,
		ReadHeaderTimeout: cfg.HTTPReadHeaderTimeout,
		ReadTimeout:       cfg.HTTPReadTimeout,
		WriteTimeout:      cfg.HTTPWriteTimeout,
		IdleTimeout:       cfg.HTTPIdleTimeout,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}

	// Bind both listeners before starting the serve goroutines so "address
	// already in use" is returned here, synchronously, instead of racing
	// with the shutdown select below.
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("listen on %q: %w", cfg.HTTPAddr, err)
	}
	mln, err := lc.Listen(ctx, "tcp", cfg.MetricsAddr)
	if err != nil {
		_ = ln.Close()
		return fmt.Errorf("listen on %q: %w", cfg.MetricsAddr, err)
	}

	// Buffered so each goroutine can always deliver its result and exit,
	// even if run has already returned on another path.
	serveErr := make(chan error, 1)
	metricsErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	go func() { metricsErr <- metricsSrv.Serve(mln) }()
	// Covers early returns; after a clean Shutdown below this is a no-op.
	defer func() { _ = metricsSrv.Close() }()
	logger.Info("server started",
		slog.String("addr", ln.Addr().String()),
		slog.String("metrics_addr", mln.Addr().String()),
		slog.String("bid_locking", cfg.BidLocking))

	// runErr is what run returns after a graceful drain: nil for a signal,
	// or the metrics server's failure. A dead metrics listener is not a
	// reason to abandon in-flight bids, so it triggers the same drain.
	var runErr error
	metricsDone := false
	select {
	case err := <-serveErr:
		// Serve always returns a non-nil error. ErrServerClosed only follows
		// Shutdown or Close, neither of which has been called yet, so any
		// error here is a real failure. The API server is gone; there is
		// nothing to drain.
		return fmt.Errorf("serve: %w", err)
	case err := <-metricsErr:
		runErr = fmt.Errorf("serve metrics: %w", err)
		metricsDone = true
		logger.Error("metrics server failed; draining the API and exiting", slog.Any("error", err))
	case <-ctx.Done():
	}

	// Restore default signal handling: a second Ctrl-C now kills the process
	// immediately instead of being swallowed while we drain.
	stop()
	logger.Info("shutdown started", slog.String("timeout", cfg.ShutdownTimeout.String()))

	// A fresh context: ctx is already cancelled, and deriving from it would
	// give Shutdown zero time. SHUTDOWN_TIMEOUT bounds the whole sequence
	// below (API drain, metrics, live handlers), not just the API drain:
	// the platform's stop timeout (compose stop_grace_period, ECS
	// stopTimeout) is set just above it, and three phases with separate
	// budgets could add up past it and get the process SIGKILLed before
	// the pool closes. (Found by the M3.5 adversarial review.)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()

	// Shutdown closes the listener, closes idle connections, and waits for
	// active requests to finish, or for shutdownCtx to expire. The metrics
	// server keeps running meanwhile, so the drain itself is observable.
	if err := srv.Shutdown(shutdownCtx); err != nil {
		// Drain timed out: cancel in-flight handlers, force-close whatever
		// is still open so the process can exit, and report failure.
		cancelRequests()
		closeErr := srv.Close()
		return errors.Join(fmt.Errorf("graceful shutdown: %w", err), closeErr)
	}

	// Serve has returned ErrServerClosed by now; anything else is a failure
	// that happened while we were shutting down.
	if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}

	// The API is drained; now stop the metrics server with whatever is left
	// of the budget. If the drain used it all, force-close instead: the
	// worst case is one lost scrape, which must not turn a clean drain of
	// the API into a failed shutdown.
	if !metricsDone {
		if err := metricsSrv.Shutdown(shutdownCtx); err != nil {
			logger.Warn("metrics server shutdown ran out of budget; closing it", slog.Any("error", err))
			_ = metricsSrv.Close()
		}
		if err := <-metricsErr; !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve metrics: %w", err)
		}
	}

	// Shutdown does not wait for hijacked (WebSocket) connections. The hub
	// was closed by RegisterOnShutdown, so every handler is now writing its
	// 1001 close frame; wait for them (bounded) before tearing down what
	// they use. (The previous hub.Count() loop never waited: Close empties
	// the rooms immediately. Found by the M3 adversarial review.)
	handlersDone := make(chan struct{})
	go func() { liveHandlers.Wait(); close(handlersDone) }()
	// Each handler's close-frame write is itself bounded by WS_WRITE_TIMEOUT;
	// this wait is bounded by what is left of the shutdown budget.
	select {
	case <-handlersDone:
	case <-shutdownCtx.Done():
		// Ask the count, not handlersDone: if the budget ran out before this
		// point, the goroutine above may not have run yet even with no
		// handlers left.
		if n := liveHandlers.Running(); n > 0 {
			logger.Warn("live handlers still running when the shutdown budget ran out", slog.Int64("running", n))
		}
	}
	stopLive()
	<-syncDone
	<-busDone

	// Every handler has returned, so no connection is in use; closing the
	// pool now ends the database sessions cleanly instead of leaving the
	// server to notice dropped connections.
	pool.Close()
	logger.Info("shutdown complete")
	return runErr
}
