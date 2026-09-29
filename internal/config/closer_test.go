package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

func validCloserEnv() map[string]string {
	return map[string]string{
		"METRICS_ADDR":          ":9091",
		"LOG_LEVEL":             "info",
		"DATABASE_URL":          "postgres://app@db:5432/app",
		"DB_IDLE_IN_TX_TIMEOUT": "5s",
		"CLOSER_INTERVAL":       "250ms",
		"CLOSER_BATCH_SIZE":     "100",
	}
}

func TestLoadCloser(t *testing.T) {
	got, err := LoadCloser(lookupFrom(validCloserEnv()))
	if err != nil {
		t.Fatalf("LoadCloser() unexpected error: %v", err)
	}
	want := CloserConfig{LogLevel: slog.LevelInfo, MetricsAddr: ":9091", DatabaseURL: "postgres://app@db:5432/app",
		DBIdleInTxTimeout: 5 * time.Second, CloserInterval: 250 * time.Millisecond, CloserBatchSize: 100}
	if got != want {
		t.Errorf("LoadCloser() = %+v, want %+v", got, want)
	}
}

func TestLoadCloserReportsEveryMissingVariable(t *testing.T) {
	_, err := LoadCloser(lookupFrom(map[string]string{}))
	if err == nil {
		t.Fatal("LoadCloser() with no env: want error")
	}
	for key := range validCloserEnv() {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error does not mention %s: %v", key, err)
		}
	}
}
