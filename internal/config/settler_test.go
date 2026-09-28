package config

import (
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"
)

func validSettlerEnv() map[string]string {
	return map[string]string{
		"LOG_LEVEL":                 "info",
		"DATABASE_URL":              "postgres://app@db:5432/app",
		"DB_IDLE_IN_TX_TIMEOUT":     "5s",
		"KAFKA_BROKERS":             "k1:9092",
		"OUTBOX_TOPIC":              "auction-events",
		"SETTLEMENT_GROUP":          "settlement",
		"SETTLEMENT_DLQ_TOPIC":      "settlement-dlq",
		"KAFKA_REPLICATION_FACTOR":  "1",
		"SETTLEMENT_RETRY_INTERVAL": "1s",
		"PAYSIM_URL":                "http://paysim:8090",
		"PAYMENT_TIMEOUT":           "2s",
		"PAYMENT_MAX_ATTEMPTS":      "6",
		"PAYMENT_BACKOFF_BASE":      "100ms",
		"PAYMENT_BACKOFF_CAP":       "5s",
		"BREAKER_FAILURE_THRESHOLD": "5",
		"BREAKER_OPEN_DURATION":     "10s",
	}
}

func TestLoadSettler(t *testing.T) {
	got, err := LoadSettler(lookupFrom(validSettlerEnv()))
	if err != nil {
		t.Fatalf("LoadSettler() unexpected error: %v", err)
	}
	want := SettlerConfig{
		LogLevel: slog.LevelInfo, DatabaseURL: "postgres://app@db:5432/app", DBIdleInTxTimeout: 5 * time.Second,
		KafkaBrokers: []string{"k1:9092"}, OutboxTopic: "auction-events", SettlementGroup: "settlement",
		SettlementDLQTopic: "settlement-dlq", KafkaReplicationFactor: 1, SettlementRetryInterval: time.Second,
		PaysimURL: "http://paysim:8090", PaymentTimeout: 2 * time.Second, PaymentMaxAttempts: 6,
		PaymentBackoffBase: 100 * time.Millisecond, PaymentBackoffCap: 5 * time.Second,
		BreakerFailureThreshold: 5, BreakerOpenDuration: 10 * time.Second,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("LoadSettler() = %+v, want %+v", got, want)
	}
}

func TestLoadSettlerRejects(t *testing.T) {
	env := validSettlerEnv()
	env["PAYMENT_BACKOFF_BASE"] = "10s"
	if _, err := LoadSettler(lookupFrom(env)); err == nil || !strings.Contains(err.Error(), "PAYMENT_BACKOFF_BASE (10s) must not exceed PAYMENT_BACKOFF_CAP (5s)") {
		t.Errorf("base above cap: error = %v", err)
	}
	_, err := LoadSettler(lookupFrom(map[string]string{}))
	for key := range validSettlerEnv() {
		if err == nil || !strings.Contains(err.Error(), key+": required") {
			t.Errorf("LoadSettler(empty) does not report %s: %v", key, err)
		}
	}
}
