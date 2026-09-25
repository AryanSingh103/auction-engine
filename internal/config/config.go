// Package config loads process configuration from environment variables.
//
// Every variable is required and there are no defaults in code: a missing or
// malformed value is a startup error. All problems are reported together so a
// misconfigured deployment can be fixed in one pass.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"time"
)

// Config is the fully validated configuration for the API process.
type Config struct {
	// HTTPAddr is the listen address, e.g. ":8080". Inside a container it
	// must not be "localhost:..." or the server is unreachable from outside.
	HTTPAddr string
	// MetricsAddr is the listen address of the separate metrics server
	// (/metrics). It is never the public port; see docs/decisions/013.
	MetricsAddr string
	// LogLevel is the minimum slog level that is emitted.
	LogLevel slog.Level
	// ShutdownTimeout bounds how long graceful shutdown waits for in-flight
	// requests before giving up.
	ShutdownTimeout time.Duration

	// HTTP server timeouts; see docs/decisions/003.
	HTTPReadHeaderTimeout time.Duration
	HTTPReadTimeout       time.Duration
	HTTPWriteTimeout      time.Duration
	HTTPIdleTimeout       time.Duration

	// RequestTimeout is the deadline put on every request's context. It must
	// be shorter than HTTPWriteTimeout so a slow handler is cancelled (and
	// answers 503) before net/http silently kills the connection.
	RequestTimeout time.Duration

	// DatabaseURL is a libpq-style connection string or URL for Postgres.
	DatabaseURL string
	// DBMaxConns caps the pgx connection pool. Every in-flight bid holds one
	// connection for the length of its transaction, so this bounds how many
	// bids can be waiting on the auction row lock at once.
	DBMaxConns int32
	// RedisURL is the Redis connection URL (redis://host:port/db).
	RedisURL string
	// RedisTimeout bounds every Redis operation. Redis is an accelerator,
	// so callers degrade on timeout instead of failing (docs/decisions/017).
	RedisTimeout time.Duration
	// AuctionCacheTTL bounds how long a cached auction state can be served
	// if a post-commit refresh was lost (docs/decisions/019).
	AuctionCacheTTL time.Duration
	// Live (WebSocket) settings; docs/decisions/020.
	WSSendBuffer     int32         // messages buffered per connection before it is dropped as slow
	WSPingInterval   time.Duration // keepalive ping; must be well under proxy idle timeouts
	WSWriteTimeout   time.Duration // bound on each message write and ping
	WSSyncInterval   time.Duration // how often each room gets the current head
	WSMaxConnections int32         // cap on concurrent live connections per instance
	// RateLimitBidsPerSecond and RateLimitBidBurst shape the per-user token
	// bucket on bid placement (docs/decisions/018).
	RateLimitBidsPerSecond float64
	RateLimitBidBurst      int32
	// BidLocking selects the bid path's concurrency strategy: "pessimistic"
	// (row lock for the whole transaction, the default in .env.example) or
	// "optimistic" (kept for benchmarking; see docs/decisions/014).
	BidLocking string
	// DBIdleInTxTimeout makes Postgres end any session that sits idle inside
	// an open transaction for longer than this. If an API host dies between
	// statements while holding an auction row lock, the lock is released
	// after this long instead of when TCP keepalive notices (~2 hours).
	DBIdleInTxTimeout time.Duration
}

// MigrateConfig is the configuration for the one-shot migrate command, which
// needs far less than the API and must not require the API's settings.
type MigrateConfig struct {
	DatabaseURL string
	LogLevel    slog.Level
}

// LookupFunc has the signature of os.LookupEnv. Taking it as a parameter lets
// tests supply a map instead of mutating the process environment.
type LookupFunc func(key string) (string, bool)

// Load reads and validates the API configuration using lookup.
func Load(lookup LookupFunc) (Config, error) {
	var cfg Config
	var errs []error

	if v, err := required(lookup, "HTTP_ADDR"); err != nil {
		errs = append(errs, err)
	} else {
		cfg.HTTPAddr = v
	}

	if v, err := required(lookup, "METRICS_ADDR"); err != nil {
		errs = append(errs, err)
	} else {
		cfg.MetricsAddr = v
	}

	if lvl, err := logLevel(lookup); err != nil {
		errs = append(errs, err)
	} else {
		cfg.LogLevel = lvl
	}

	durations := []struct {
		key string
		dst *time.Duration
	}{
		{"SHUTDOWN_TIMEOUT", &cfg.ShutdownTimeout},
		{"HTTP_READ_HEADER_TIMEOUT", &cfg.HTTPReadHeaderTimeout},
		{"HTTP_READ_TIMEOUT", &cfg.HTTPReadTimeout},
		{"HTTP_WRITE_TIMEOUT", &cfg.HTTPWriteTimeout},
		{"HTTP_IDLE_TIMEOUT", &cfg.HTTPIdleTimeout},
		{"REQUEST_TIMEOUT", &cfg.RequestTimeout},
		{"DB_IDLE_IN_TX_TIMEOUT", &cfg.DBIdleInTxTimeout},
		{"REDIS_TIMEOUT", &cfg.RedisTimeout},
		{"AUCTION_CACHE_TTL", &cfg.AuctionCacheTTL},
		{"WS_PING_INTERVAL", &cfg.WSPingInterval},
		{"WS_WRITE_TIMEOUT", &cfg.WSWriteTimeout},
		{"WS_SYNC_INTERVAL", &cfg.WSSyncInterval},
	}
	durationsOK := true
	for _, d := range durations {
		v, err := positiveDuration(lookup, d.key)
		if err != nil {
			errs = append(errs, err)
			durationsOK = false
			continue
		}
		*d.dst = v
	}

	// Cross-field rules. Only checked when every duration parsed, so one bad
	// value does not produce a second, confusing error about its relations.
	if durationsOK {
		if cfg.HTTPReadHeaderTimeout > cfg.HTTPReadTimeout {
			errs = append(errs, fmt.Errorf("HTTP_READ_HEADER_TIMEOUT (%s) must not exceed HTTP_READ_TIMEOUT (%s)",
				cfg.HTTPReadHeaderTimeout, cfg.HTTPReadTimeout))
		}
		if cfg.RequestTimeout >= cfg.HTTPWriteTimeout {
			errs = append(errs, fmt.Errorf("REQUEST_TIMEOUT (%s) must be less than HTTP_WRITE_TIMEOUT (%s)",
				cfg.RequestTimeout, cfg.HTTPWriteTimeout))
		}
	}

	if v, err := required(lookup, "DATABASE_URL"); err != nil {
		errs = append(errs, err)
	} else {
		cfg.DatabaseURL = v
	}

	if v, err := required(lookup, "REDIS_URL"); err != nil {
		errs = append(errs, err)
	} else {
		cfg.RedisURL = v
	}

	if n, err := positiveInt32(lookup, "WS_MAX_CONNECTIONS"); err != nil {
		errs = append(errs, err)
	} else {
		cfg.WSMaxConnections = n
	}

	if n, err := positiveInt32(lookup, "WS_SEND_BUFFER"); err != nil {
		errs = append(errs, err)
	} else {
		cfg.WSSendBuffer = n
	}

	if v, err := positiveFloat(lookup, "RATE_LIMIT_BIDS_PER_SECOND"); err != nil {
		errs = append(errs, err)
	} else {
		cfg.RateLimitBidsPerSecond = v
	}
	if n, err := positiveInt32(lookup, "RATE_LIMIT_BID_BURST"); err != nil {
		errs = append(errs, err)
	} else {
		cfg.RateLimitBidBurst = n
	}

	if v, err := required(lookup, "BID_LOCKING"); err != nil {
		errs = append(errs, err)
	} else if v != "pessimistic" && v != "optimistic" {
		errs = append(errs, fmt.Errorf("BID_LOCKING: invalid value %q: must be pessimistic or optimistic", v))
	} else {
		cfg.BidLocking = v
	}

	if n, err := positiveInt32(lookup, "DB_MAX_CONNS"); err != nil {
		errs = append(errs, err)
	} else {
		cfg.DBMaxConns = n
	}

	if len(errs) > 0 {
		return Config{}, errors.Join(errs...)
	}
	return cfg, nil
}

// LoadMigrate reads and validates the migrate command's configuration.
func LoadMigrate(lookup LookupFunc) (MigrateConfig, error) {
	var cfg MigrateConfig
	var errs []error

	if v, err := required(lookup, "DATABASE_URL"); err != nil {
		errs = append(errs, err)
	} else {
		cfg.DatabaseURL = v
	}
	if lvl, err := logLevel(lookup); err != nil {
		errs = append(errs, err)
	} else {
		cfg.LogLevel = lvl
	}

	if len(errs) > 0 {
		return MigrateConfig{}, errors.Join(errs...)
	}
	return cfg, nil
}

// required returns the value of key, treating unset and empty as missing.
// Empty counts as missing because `KEY=` in an env file is almost always a
// mistake, not a deliberate empty setting.
func required(lookup LookupFunc, key string) (string, error) {
	v, ok := lookup(key)
	if !ok || v == "" {
		return "", fmt.Errorf("%s: required but not set", key)
	}
	return v, nil
}

func logLevel(lookup LookupFunc) (slog.Level, error) {
	v, err := required(lookup, "LOG_LEVEL")
	if err != nil {
		return 0, err
	}
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(v)); err != nil {
		return 0, fmt.Errorf("LOG_LEVEL: invalid value %q: must be debug, info, warn or error", v)
	}
	return lvl, nil
}

// positiveDuration parses key as a Go duration ("15s", "500ms") that must be
// greater than zero. Zero is rejected because for net/http timeouts it means
// "no timeout", which is exactly the unbounded behavior these settings exist
// to prevent.
func positiveDuration(lookup LookupFunc, key string) (time.Duration, error) {
	v, err := required(lookup, key)
	if err != nil {
		return 0, err
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid duration %q: %w", key, v, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s: must be positive, got %s", key, d)
	}
	return d, nil
}

// positiveInt32 parses key as a base-10 integer in [1, MaxInt32]. int32
// because that is what pgxpool.Config.MaxConns takes.
func positiveInt32(lookup LookupFunc, key string) (int32, error) {
	v, err := required(lookup, key)
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(v, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid integer %q", key, v)
	}
	if n <= 0 {
		return 0, fmt.Errorf("%s: must be positive, got %d", key, n)
	}
	return int32(n), nil
}

// positiveFloat parses key as a decimal number greater than zero.
func positiveFloat(lookup LookupFunc, key string) (float64, error) {
	v, err := required(lookup, key)
	if err != nil {
		return 0, err
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, fmt.Errorf("%s: invalid number %q", key, v)
	}
	if f <= 0 {
		return 0, fmt.Errorf("%s: must be positive, got %v", key, f)
	}
	return f, nil
}
