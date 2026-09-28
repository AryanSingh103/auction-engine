package config

import (
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// SettlerConfig is the configuration for the settlement consumer
// (cmd/settler, docs/decisions/024).
type SettlerConfig struct {
	LogLevel          slog.Level
	DatabaseURL       string
	DBIdleInTxTimeout time.Duration
	KafkaBrokers      []string
	// OutboxTopic is consumed as consumer group SettlementGroup. Events
	// that cannot be settled go to SettlementDLQTopic, which the settler
	// creates (one partition, KafkaReplicationFactor replicas) if missing.
	OutboxTopic            string
	SettlementGroup        string
	SettlementDLQTopic     string
	KafkaReplicationFactor int16
	// SettlementRetryInterval is the wait before retrying an event after a
	// database error.
	SettlementRetryInterval time.Duration
	// PaysimURL is the payment provider's base URL.
	PaysimURL string
	// PaymentTimeout bounds one payment attempt; PaymentMaxAttempts is how
	// many attempts an invoice gets per delivery, with full-jitter backoff
	// from PaymentBackoffBase up to PaymentBackoffCap between them.
	PaymentTimeout     time.Duration
	PaymentMaxAttempts int32
	PaymentBackoffBase time.Duration
	PaymentBackoffCap  time.Duration
	// The circuit breaker opens after BreakerFailureThreshold consecutive
	// failures and stays open for BreakerOpenDuration.
	BreakerFailureThreshold int32
	BreakerOpenDuration     time.Duration
}

// LoadSettler reads and validates SettlerConfig, reporting every problem.
func LoadSettler(lookup LookupFunc) (SettlerConfig, error) {
	var cfg SettlerConfig
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
		{"DATABASE_URL", &cfg.DatabaseURL},
		{"OUTBOX_TOPIC", &cfg.OutboxTopic},
		{"SETTLEMENT_GROUP", &cfg.SettlementGroup},
		{"SETTLEMENT_DLQ_TOPIC", &cfg.SettlementDLQTopic},
		{"PAYSIM_URL", &cfg.PaysimURL},
	} {
		if v, err := required(lookup, s.key); err != nil {
			errs = append(errs, err)
		} else {
			*s.dst = v
		}
	}
	if b, err := brokers(lookup); err != nil {
		errs = append(errs, err)
	} else {
		cfg.KafkaBrokers = b
	}
	if v, err := replicationFactor(lookup); err != nil {
		errs = append(errs, err)
	} else {
		cfg.KafkaReplicationFactor = v
	}
	for _, n := range []struct {
		key string
		dst *int32
	}{
		{"PAYMENT_MAX_ATTEMPTS", &cfg.PaymentMaxAttempts},
		{"BREAKER_FAILURE_THRESHOLD", &cfg.BreakerFailureThreshold},
	} {
		if v, err := positiveInt32(lookup, n.key); err != nil {
			errs = append(errs, err)
		} else {
			*n.dst = v
		}
	}
	durationsOK := true
	for _, d := range []struct {
		key string
		dst *time.Duration
	}{
		{"DB_IDLE_IN_TX_TIMEOUT", &cfg.DBIdleInTxTimeout},
		{"SETTLEMENT_RETRY_INTERVAL", &cfg.SettlementRetryInterval},
		{"PAYMENT_TIMEOUT", &cfg.PaymentTimeout},
		{"PAYMENT_BACKOFF_BASE", &cfg.PaymentBackoffBase},
		{"PAYMENT_BACKOFF_CAP", &cfg.PaymentBackoffCap},
		{"BREAKER_OPEN_DURATION", &cfg.BreakerOpenDuration},
	} {
		v, err := positiveDuration(lookup, d.key)
		if err != nil {
			errs = append(errs, err)
			durationsOK = false
			continue
		}
		*d.dst = v
	}
	if durationsOK && cfg.PaymentBackoffBase > cfg.PaymentBackoffCap {
		errs = append(errs, fmt.Errorf("PAYMENT_BACKOFF_BASE (%s) must not exceed PAYMENT_BACKOFF_CAP (%s)",
			cfg.PaymentBackoffBase, cfg.PaymentBackoffCap))
	}

	if len(errs) > 0 {
		return SettlerConfig{}, errors.Join(errs...)
	}
	return cfg, nil
}
