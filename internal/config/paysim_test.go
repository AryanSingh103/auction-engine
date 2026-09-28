package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

func validPaysimEnv() map[string]string {
	return map[string]string{
		"LOG_LEVEL":            "info",
		"PAYSIM_ADDR":          ":8090",
		"DATABASE_URL":         "postgres://app@db:5432/app",
		"PAYSIM_FAILURE_RATE":  "0.1",
		"PAYSIM_HANG_RATE":     "0",
		"PAYSIM_HANG_DURATION": "5s",
	}
}

func TestLoadPaysim(t *testing.T) {
	got, err := LoadPaysim(lookupFrom(validPaysimEnv()))
	if err != nil {
		t.Fatalf("LoadPaysim() unexpected error: %v", err)
	}
	want := PaysimConfig{LogLevel: slog.LevelInfo, Addr: ":8090", DatabaseURL: "postgres://app@db:5432/app",
		FailureRate: 0.1, HangRate: 0, HangDuration: 5 * time.Second}
	if got != want {
		t.Errorf("LoadPaysim() = %+v, want %+v", got, want)
	}
}

func TestLoadPaysimRejects(t *testing.T) {
	for _, tt := range []struct{ key, value, wantErr string }{
		{"PAYSIM_FAILURE_RATE", "1.5", "must be a number from 0 to 1"},
		{"PAYSIM_FAILURE_RATE", "-0.1", "must be a number from 0 to 1"},
		{"PAYSIM_HANG_RATE", "0.95", "must not exceed 1"},
		{"PAYSIM_HANG_DURATION", "0s", "PAYSIM_HANG_DURATION"},
	} {
		env := validPaysimEnv()
		env[tt.key] = tt.value
		if _, err := LoadPaysim(lookupFrom(env)); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
			t.Errorf("%s=%s: error = %v, want it to contain %q", tt.key, tt.value, err, tt.wantErr)
		}
	}
	_, err := LoadPaysim(lookupFrom(map[string]string{}))
	for key := range validPaysimEnv() {
		if err == nil || !strings.Contains(err.Error(), key+": required") {
			t.Errorf("LoadPaysim(empty) does not report %s: %v", key, err)
		}
	}
}
