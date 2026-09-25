package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

// validEnv returns a complete, valid environment. Each test case starts from a
// fresh copy and changes only what it is testing.
func validEnv() map[string]string {
	return map[string]string{
		"HTTP_ADDR":        ":8080",
		"METRICS_ADDR":     ":9091",
		"LOG_LEVEL":        "info",
		"SHUTDOWN_TIMEOUT": "15s",

		"HTTP_READ_HEADER_TIMEOUT": "5s",
		"HTTP_READ_TIMEOUT":        "10s",
		"HTTP_WRITE_TIMEOUT":       "10s",
		"HTTP_IDLE_TIMEOUT":        "60s",
		"REQUEST_TIMEOUT":          "5s",

		"DATABASE_URL": "postgres://app@db:5432/app",
		"DB_MAX_CONNS": "20",
		"BID_LOCKING":  "pessimistic",

		"DB_IDLE_IN_TX_TIMEOUT": "5s",
		"REDIS_URL":             "redis://cache:6379/0",
		"REDIS_TIMEOUT":         "200ms",
		"AUCTION_CACHE_TTL":     "10s",
		"WS_SEND_BUFFER":        "64",
		"WS_PING_INTERVAL":      "20s",
		"WS_WRITE_TIMEOUT":      "5s",
		"WS_SYNC_INTERVAL":      "5s",

		"RATE_LIMIT_BIDS_PER_SECOND": "10",
		"RATE_LIMIT_BID_BURST":       "20",
	}
}

// validConfig is what validEnv must parse to.
func validConfig() Config {
	return Config{
		HTTPAddr:               ":8080",
		MetricsAddr:            ":9091",
		LogLevel:               slog.LevelInfo,
		ShutdownTimeout:        15 * time.Second,
		HTTPReadHeaderTimeout:  5 * time.Second,
		HTTPReadTimeout:        10 * time.Second,
		HTTPWriteTimeout:       10 * time.Second,
		HTTPIdleTimeout:        60 * time.Second,
		RequestTimeout:         5 * time.Second,
		DatabaseURL:            "postgres://app@db:5432/app",
		DBMaxConns:             20,
		BidLocking:             "pessimistic",
		DBIdleInTxTimeout:      5 * time.Second,
		RedisURL:               "redis://cache:6379/0",
		RedisTimeout:           200 * time.Millisecond,
		AuctionCacheTTL:        10 * time.Second,
		WSSendBuffer:           64,
		WSPingInterval:         20 * time.Second,
		WSWriteTimeout:         5 * time.Second,
		WSSyncInterval:         5 * time.Second,
		RateLimitBidsPerSecond: 10,
		RateLimitBidBurst:      20,
	}
}

func lookupFrom(env map[string]string) LookupFunc {
	return func(key string) (string, bool) {
		v, ok := env[key]
		return v, ok
	}
}

func TestLoad(t *testing.T) {
	tests := []struct {
		name string
		// mutate edits a copy of validEnv. A nil mutate means "use it as is".
		mutate func(env map[string]string)
		want   Config
		// wantErrs lists substrings that must all appear in the error.
		// Empty means Load must succeed.
		wantErrs []string
	}{
		{
			name: "valid",
			want: validConfig(),
		},
		{
			name:   "log level is case-insensitive",
			mutate: func(env map[string]string) { env["LOG_LEVEL"] = "DEBUG" },
			want: func() Config {
				c := validConfig()
				c.LogLevel = slog.LevelDebug
				return c
			}(),
		},
		{
			name:     "missing HTTP_ADDR",
			mutate:   func(env map[string]string) { delete(env, "HTTP_ADDR") },
			wantErrs: []string{"HTTP_ADDR: required but not set"},
		},
		{
			name:     "missing LOG_LEVEL",
			mutate:   func(env map[string]string) { delete(env, "LOG_LEVEL") },
			wantErrs: []string{"LOG_LEVEL: required but not set"},
		},
		{
			name:     "missing SHUTDOWN_TIMEOUT",
			mutate:   func(env map[string]string) { delete(env, "SHUTDOWN_TIMEOUT") },
			wantErrs: []string{"SHUTDOWN_TIMEOUT: required but not set"},
		},
		{
			name:     "missing HTTP_WRITE_TIMEOUT",
			mutate:   func(env map[string]string) { delete(env, "HTTP_WRITE_TIMEOUT") },
			wantErrs: []string{"HTTP_WRITE_TIMEOUT: required but not set"},
		},
		{
			// Zero means "no timeout" to net/http, so it must be rejected.
			name:     "zero server timeout",
			mutate:   func(env map[string]string) { env["HTTP_IDLE_TIMEOUT"] = "0" },
			wantErrs: []string{"HTTP_IDLE_TIMEOUT: must be positive"},
		},
		{
			name:     "empty value counts as missing",
			mutate:   func(env map[string]string) { env["HTTP_ADDR"] = "" },
			wantErrs: []string{"HTTP_ADDR: required but not set"},
		},
		{
			name:     "unknown log level",
			mutate:   func(env map[string]string) { env["LOG_LEVEL"] = "verbose" },
			wantErrs: []string{`LOG_LEVEL: invalid value "verbose"`},
		},
		{
			name:     "duration without unit",
			mutate:   func(env map[string]string) { env["SHUTDOWN_TIMEOUT"] = "15" },
			wantErrs: []string{`SHUTDOWN_TIMEOUT: invalid duration "15"`},
		},
		{
			name:     "zero duration",
			mutate:   func(env map[string]string) { env["SHUTDOWN_TIMEOUT"] = "0s" },
			wantErrs: []string{"SHUTDOWN_TIMEOUT: must be positive"},
		},
		{
			name:     "negative duration",
			mutate:   func(env map[string]string) { env["SHUTDOWN_TIMEOUT"] = "-5s" },
			wantErrs: []string{"SHUTDOWN_TIMEOUT: must be positive"},
		},
		{
			name:     "rate that is not a number",
			mutate:   func(env map[string]string) { env["RATE_LIMIT_BIDS_PER_SECOND"] = "fast" },
			wantErrs: []string{`RATE_LIMIT_BIDS_PER_SECOND: invalid number "fast"`},
		},
		{
			name:     "infinite rate",
			mutate:   func(env map[string]string) { env["RATE_LIMIT_BIDS_PER_SECOND"] = "Inf" },
			wantErrs: []string{`RATE_LIMIT_BIDS_PER_SECOND: invalid number "Inf"`},
		},
		{
			name:     "unknown BID_LOCKING",
			mutate:   func(env map[string]string) { env["BID_LOCKING"] = "hopeful" },
			wantErrs: []string{`BID_LOCKING: invalid value "hopeful"`},
		},
		{
			name:     "missing DATABASE_URL",
			mutate:   func(env map[string]string) { delete(env, "DATABASE_URL") },
			wantErrs: []string{"DATABASE_URL: required but not set"},
		},
		{
			name:     "non-numeric DB_MAX_CONNS",
			mutate:   func(env map[string]string) { env["DB_MAX_CONNS"] = "lots" },
			wantErrs: []string{`DB_MAX_CONNS: invalid integer "lots"`},
		},
		{
			name:     "zero DB_MAX_CONNS",
			mutate:   func(env map[string]string) { env["DB_MAX_CONNS"] = "0" },
			wantErrs: []string{"DB_MAX_CONNS: must be positive"},
		},
		{
			name:     "DB_MAX_CONNS beyond int32",
			mutate:   func(env map[string]string) { env["DB_MAX_CONNS"] = "3000000000" },
			wantErrs: []string{`DB_MAX_CONNS: invalid integer "3000000000"`},
		},
		{
			// A request deadline at or past the write timeout would let
			// net/http kill the connection before the handler is cancelled.
			name:     "REQUEST_TIMEOUT equal to HTTP_WRITE_TIMEOUT",
			mutate:   func(env map[string]string) { env["REQUEST_TIMEOUT"] = "10s" },
			wantErrs: []string{"REQUEST_TIMEOUT (10s) must be less than HTTP_WRITE_TIMEOUT (10s)"},
		},
		{
			name:     "read header timeout above read timeout",
			mutate:   func(env map[string]string) { env["HTTP_READ_HEADER_TIMEOUT"] = "11s" },
			wantErrs: []string{"HTTP_READ_HEADER_TIMEOUT (11s) must not exceed HTTP_READ_TIMEOUT (10s)"},
		},
		{
			name: "all problems reported at once",
			mutate: func(env map[string]string) {
				delete(env, "HTTP_ADDR")
				env["LOG_LEVEL"] = "loud"
				env["SHUTDOWN_TIMEOUT"] = "soon"
			},
			wantErrs: []string{
				"HTTP_ADDR: required but not set",
				`LOG_LEVEL: invalid value "loud"`,
				`SHUTDOWN_TIMEOUT: invalid duration "soon"`,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := validEnv()
			if tt.mutate != nil {
				tt.mutate(env)
			}

			got, err := Load(lookupFrom(env))

			if len(tt.wantErrs) == 0 {
				if err != nil {
					t.Fatalf("Load() unexpected error: %v", err)
				}
				if got != tt.want {
					t.Fatalf("Load() = %+v, want %+v", got, tt.want)
				}
				return
			}

			if err == nil {
				t.Fatalf("Load() error = nil, want errors containing %q", tt.wantErrs)
			}
			for _, want := range tt.wantErrs {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Load() error = %q, missing %q", err, want)
				}
			}
			if got != (Config{}) {
				t.Errorf("Load() returned partial config %+v alongside an error, want zero value", got)
			}
		})
	}
}

func TestLoadSkipsCrossFieldChecksWhenADurationIsInvalid(t *testing.T) {
	env := validEnv()
	env["HTTP_WRITE_TIMEOUT"] = "never"

	_, err := Load(lookupFrom(env))

	if err == nil {
		t.Fatal("Load() error = nil, want error")
	}
	if strings.Contains(err.Error(), "must be less than") {
		t.Errorf("error mentions a cross-field rule although a duration failed to parse: %q", err)
	}
}

func TestLoadMigrate(t *testing.T) {
	got, err := LoadMigrate(lookupFrom(map[string]string{
		"DATABASE_URL": "postgres://app@db:5432/app",
		"LOG_LEVEL":    "warn",
	}))
	if err != nil {
		t.Fatalf("LoadMigrate() unexpected error: %v", err)
	}
	want := MigrateConfig{DatabaseURL: "postgres://app@db:5432/app", LogLevel: slog.LevelWarn}
	if got != want {
		t.Errorf("LoadMigrate() = %+v, want %+v", got, want)
	}

	_, err = LoadMigrate(lookupFrom(map[string]string{}))
	if err == nil || !strings.Contains(err.Error(), "DATABASE_URL: required") || !strings.Contains(err.Error(), "LOG_LEVEL: required") {
		t.Errorf("LoadMigrate(empty) error = %v, want both variables reported", err)
	}
}
