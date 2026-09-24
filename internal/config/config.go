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
	"time"
)

// Config is the fully validated configuration for the API process.
type Config struct {
	// HTTPAddr is the listen address, e.g. ":8080". Inside a container it
	// must not be "localhost:..." or the server is unreachable from outside.
	HTTPAddr string
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
}

// LookupFunc has the signature of os.LookupEnv. Taking it as a parameter lets
// tests supply a map instead of mutating the process environment.
type LookupFunc func(key string) (string, bool)

// Load reads and validates all configuration using lookup.
func Load(lookup LookupFunc) (Config, error) {
	var cfg Config
	var errs []error

	if v, err := required(lookup, "HTTP_ADDR"); err != nil {
		errs = append(errs, err)
	} else {
		cfg.HTTPAddr = v
	}

	if v, err := required(lookup, "LOG_LEVEL"); err != nil {
		errs = append(errs, err)
	} else if err := cfg.LogLevel.UnmarshalText([]byte(v)); err != nil {
		errs = append(errs, fmt.Errorf("LOG_LEVEL: invalid value %q: must be debug, info, warn or error", v))
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
	}
	for _, d := range durations {
		v, err := positiveDuration(lookup, d.key)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		*d.dst = v
	}

	if len(errs) > 0 {
		return Config{}, errors.Join(errs...)
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
