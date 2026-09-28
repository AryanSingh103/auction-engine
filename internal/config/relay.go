package config

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// RelayConfig is the configuration for the outbox relay (cmd/relay,
// docs/decisions/022).
type RelayConfig struct {
	LogLevel    slog.Level
	DatabaseURL string
	// DBIdleInTxTimeout also bounds a stuck publish: the relay holds its
	// transaction open while Kafka acknowledges.
	DBIdleInTxTimeout time.Duration
	// KafkaBrokers is the seed broker list (KAFKA_BROKERS, comma-separated).
	KafkaBrokers []string
	// OutboxTopic is where events are published. The relay creates it with
	// OutboxTopicPartitions partitions and KafkaReplicationFactor replicas
	// if it does not exist; an existing topic is left as it is.
	OutboxTopic            string
	OutboxTopicPartitions  int32
	KafkaReplicationFactor int32
	// RelayBatchSize is the most events published per transaction.
	RelayBatchSize int32
	// RelayPollInterval is the wait after a batch that was not full.
	RelayPollInterval time.Duration
	// RelayPublishTimeout bounds each batch's wait for acknowledgements. It
	// must be below DBIdleInTxTimeout, so the relay gives up on its own
	// before Postgres ends its session.
	RelayPublishTimeout time.Duration
}

// LoadRelay reads and validates RelayConfig, reporting every problem.
func LoadRelay(lookup LookupFunc) (RelayConfig, error) {
	var cfg RelayConfig
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
	for _, n := range []struct {
		key string
		dst *int32
	}{
		{"OUTBOX_TOPIC_PARTITIONS", &cfg.OutboxTopicPartitions},
		{"KAFKA_REPLICATION_FACTOR", &cfg.KafkaReplicationFactor},
		{"RELAY_BATCH_SIZE", &cfg.RelayBatchSize},
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
		{"RELAY_POLL_INTERVAL", &cfg.RelayPollInterval},
		{"RELAY_PUBLISH_TIMEOUT", &cfg.RelayPublishTimeout},
	} {
		v, err := positiveDuration(lookup, d.key)
		if err != nil {
			errs = append(errs, err)
			durationsOK = false
			continue
		}
		*d.dst = v
	}
	if durationsOK && cfg.RelayPublishTimeout >= cfg.DBIdleInTxTimeout {
		errs = append(errs, fmt.Errorf("RELAY_PUBLISH_TIMEOUT (%s) must be less than DB_IDLE_IN_TX_TIMEOUT (%s)",
			cfg.RelayPublishTimeout, cfg.DBIdleInTxTimeout))
	}

	if len(errs) > 0 {
		return RelayConfig{}, errors.Join(errs...)
	}
	return cfg, nil
}

// brokers parses KAFKA_BROKERS: comma-separated host:port entries, none
// empty.
func brokers(lookup LookupFunc) ([]string, error) {
	v, err := required(lookup, "KAFKA_BROKERS")
	if err != nil {
		return nil, err
	}
	parts := strings.Split(v, ",")
	for i, p := range parts {
		parts[i] = strings.TrimSpace(p)
		if parts[i] == "" {
			return nil, fmt.Errorf("KAFKA_BROKERS: empty entry in %q", v)
		}
	}
	return parts, nil
}
