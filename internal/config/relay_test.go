package config

import (
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"
)

func validRelayEnv() map[string]string {
	return map[string]string{
		"LOG_LEVEL":                "info",
		"DATABASE_URL":             "postgres://app@db:5432/app",
		"DB_IDLE_IN_TX_TIMEOUT":    "5s",
		"KAFKA_BROKERS":            "k1:9092, k2:9092",
		"OUTBOX_TOPIC":             "auction-events",
		"OUTBOX_TOPIC_PARTITIONS":  "6",
		"KAFKA_REPLICATION_FACTOR": "1",
		"RELAY_BATCH_SIZE":         "100",
		"RELAY_POLL_INTERVAL":      "100ms",
		"RELAY_PUBLISH_TIMEOUT":    "3s",
	}
}

func TestLoadRelay(t *testing.T) {
	got, err := LoadRelay(lookupFrom(validRelayEnv()))
	if err != nil {
		t.Fatalf("LoadRelay() unexpected error: %v", err)
	}
	want := RelayConfig{
		LogLevel: slog.LevelInfo, DatabaseURL: "postgres://app@db:5432/app", DBIdleInTxTimeout: 5 * time.Second,
		KafkaBrokers: []string{"k1:9092", "k2:9092"}, OutboxTopic: "auction-events", OutboxTopicPartitions: 6,
		KafkaReplicationFactor: 1, RelayBatchSize: 100, RelayPollInterval: 100 * time.Millisecond,
		RelayPublishTimeout: 3 * time.Second,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("LoadRelay() = %+v, want %+v", got, want)
	}
}

func TestLoadRelayRejects(t *testing.T) {
	tests := []struct {
		name, key, value, wantErr string
	}{
		{"missing brokers", "KAFKA_BROKERS", "", "KAFKA_BROKERS: required"},
		{"empty broker entry", "KAFKA_BROKERS", "k1:9092,,k2:9092", "KAFKA_BROKERS: empty entry"},
		{"zero batch", "RELAY_BATCH_SIZE", "0", "RELAY_BATCH_SIZE"},
		{"replication beyond int16", "KAFKA_REPLICATION_FACTOR", "40000", "KAFKA_REPLICATION_FACTOR: 40000 is above 32767"},
		{"publish timeout not below idle-in-tx", "RELAY_PUBLISH_TIMEOUT", "5s", "RELAY_PUBLISH_TIMEOUT (5s) must be less than DB_IDLE_IN_TX_TIMEOUT (5s)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := validRelayEnv()
			env[tt.key] = tt.value
			if _, err := LoadRelay(lookupFrom(env)); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}

	_, err := LoadRelay(lookupFrom(map[string]string{}))
	for key := range validRelayEnv() {
		if err == nil || !strings.Contains(err.Error(), key+": required") {
			t.Errorf("LoadRelay(empty) error does not report %s: %v", key, err)
		}
	}
}
