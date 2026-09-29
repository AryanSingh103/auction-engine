package config

import (
	"errors"
	"log/slog"
	"time"
)

// CloserConfig is the configuration for the closer (cmd/closer,
// docs/decisions/027).
type CloserConfig struct {
	LogLevel          slog.Level
	MetricsAddr       string // METRICS_ADDR: Prometheus /metrics listener
	DatabaseURL       string
	DBIdleInTxTimeout time.Duration
	// CloserInterval is how often the leader looks for ended auctions, and
	// how often a standby retries the leader lock. An auction closes at
	// most about this long after its end (bids are refused from end_at on
	// regardless: the close only starts settlement).
	CloserInterval time.Duration
	// CloserBatchSize is the most auctions closed per scan.
	CloserBatchSize int32
}

// LoadCloser reads and validates CloserConfig, reporting every problem.
func LoadCloser(lookup LookupFunc) (CloserConfig, error) {
	var cfg CloserConfig
	var errs []error

	if lvl, err := logLevel(lookup); err != nil {
		errs = append(errs, err)
	} else {
		cfg.LogLevel = lvl
	}
	for _, s := range []struct {
		key string
		dst *string
	}{
		{"METRICS_ADDR", &cfg.MetricsAddr},
		{"DATABASE_URL", &cfg.DatabaseURL},
	} {
		if v, err := required(lookup, s.key); err != nil {
			errs = append(errs, err)
		} else {
			*s.dst = v
		}
	}
	for _, d := range []struct {
		key string
		dst *time.Duration
	}{
		{"DB_IDLE_IN_TX_TIMEOUT", &cfg.DBIdleInTxTimeout},
		{"CLOSER_INTERVAL", &cfg.CloserInterval},
	} {
		if v, err := positiveDuration(lookup, d.key); err != nil {
			errs = append(errs, err)
		} else {
			*d.dst = v
		}
	}
	if v, err := positiveInt32(lookup, "CLOSER_BATCH_SIZE"); err != nil {
		errs = append(errs, err)
	} else {
		cfg.CloserBatchSize = v
	}

	if len(errs) > 0 {
		return CloserConfig{}, errors.Join(errs...)
	}
	return cfg, nil
}
