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

	if v, err := required(lookup, "SHUTDOWN_TIMEOUT"); err != nil {
		errs = append(errs, err)
	} else if d, err := time.ParseDuration(v); err != nil {
		errs = append(errs, fmt.Errorf("SHUTDOWN_TIMEOUT: invalid duration %q: %w", v, err))
	} else if d <= 0 {
		errs = append(errs, fmt.Errorf("SHUTDOWN_TIMEOUT: must be positive, got %s", d))
	} else {
		cfg.ShutdownTimeout = d
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
