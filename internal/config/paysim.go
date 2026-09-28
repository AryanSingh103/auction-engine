package config

import (
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"
)

// PaysimConfig is the configuration for the fake payment provider
// (cmd/paysim, docs/decisions/023).
type PaysimConfig struct {
	LogLevel    slog.Level
	Addr        string // PAYSIM_ADDR
	DatabaseURL string
	// FailureRate and HangRate are shares of requests in [0, 1] whose sum
	// is at most 1; see paysim.Faults. HangDuration is how long a hang lasts.
	FailureRate  float64
	HangRate     float64
	HangDuration time.Duration
}

// LoadPaysim reads and validates PaysimConfig, reporting every problem.
func LoadPaysim(lookup LookupFunc) (PaysimConfig, error) {
	var cfg PaysimConfig
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
		{"PAYSIM_ADDR", &cfg.Addr},
		{"DATABASE_URL", &cfg.DatabaseURL},
	} {
		if v, err := required(lookup, s.key); err != nil {
			errs = append(errs, err)
		} else {
			*s.dst = v
		}
	}
	ratesOK := true
	for _, f := range []struct {
		key string
		dst *float64
	}{
		{"PAYSIM_FAILURE_RATE", &cfg.FailureRate},
		{"PAYSIM_HANG_RATE", &cfg.HangRate},
	} {
		v, err := fraction(lookup, f.key)
		if err != nil {
			errs = append(errs, err)
			ratesOK = false
			continue
		}
		*f.dst = v
	}
	if ratesOK && cfg.FailureRate+cfg.HangRate > 1 {
		errs = append(errs, fmt.Errorf("PAYSIM_FAILURE_RATE + PAYSIM_HANG_RATE (%g) must not exceed 1", cfg.FailureRate+cfg.HangRate))
	}
	if d, err := positiveDuration(lookup, "PAYSIM_HANG_DURATION"); err != nil {
		errs = append(errs, err)
	} else {
		cfg.HangDuration = d
	}

	if len(errs) > 0 {
		return PaysimConfig{}, errors.Join(errs...)
	}
	return cfg, nil
}

// fraction parses a required number in [0, 1].
func fraction(lookup LookupFunc, key string) (float64, error) {
	v, err := required(lookup, key)
	if err != nil {
		return 0, err
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 || f > 1 {
		return 0, fmt.Errorf("%s: invalid value %q: must be a number from 0 to 1", key, v)
	}
	return f, nil
}
