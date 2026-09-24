package loadgen

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/AryanSingh103/auction-engine/internal/auction"
	"github.com/AryanSingh103/auction-engine/internal/httpapi"
	"github.com/AryanSingh103/auction-engine/internal/metrics"
	"github.com/AryanSingh103/auction-engine/internal/testdb"
)

var server *testdb.Server

func TestMain(m *testing.M) {
	ctx := context.Background()
	var err error
	server, err = testdb.Start(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "loadgen tests: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	if err := server.Terminate(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "loadgen tests: terminate: %v\n", err)
	}
	os.Exit(code)
}

func TestSumMetric(t *testing.T) {
	text := `# HELP http_requests_total x
# TYPE http_requests_total counter
http_requests_total{method="GET",route="/healthz",status="200"} 12
http_requests_total{method="POST",route="/auctions/{auctionID}/bids",status="409"} 30
http_requests_total_other 999
`
	got, err := sumMetric(strings.NewReader(text), "http_requests_total")
	if err != nil || got != 42 {
		t.Errorf("sumMetric = %v, %v; want 42", got, err)
	}
	// A server that has served nothing exposes no samples: that is 0.
	if got, err := sumMetric(strings.NewReader("go_goroutines 3\n"), "http_requests_total"); err != nil || got != 0 {
		t.Errorf("sumMetric on an absent metric = %v, %v; want 0, nil", got, err)
	}
}

func TestCheckServerSawRequests(t *testing.T) {
	tests := []struct {
		name            string
		delta           float64
		responses, errs int64
		pass            bool
	}{
		{"exact match", 100, 100, 0, true},
		{"server saw a request whose response was lost", 101, 100, 1, true},
		{"server saw fewer than the client received", 90, 100, 0, false},
		{"nothing reached the server", 0, 100, 0, false},
		{"more extra than transport errors explain", 105, 100, 2, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := checkServerSawRequests(tt.delta, tt.responses, tt.errs).Passed; got != tt.pass {
				t.Errorf("passed = %v, want %v", got, tt.pass)
			}
		})
	}
}

func shortConfig(label, apiURL, metricsURL string) Config {
	return Config{
		Mode: ModeBids, APIURL: apiURL, MetricsURL: metricsURL,
		Workers: 8, Auctions: 1, Warmup: 300 * time.Millisecond, Duration: 1500 * time.Millisecond,
		RaiseSteps: 3, RequestTimeout: 5 * time.Second, MaxErrorRate: 0.01, Label: label,
	}
}

// A short run against the real API (in-process, real Postgres) must pass
// every check.
func TestRunAgainstRealAPI(t *testing.T) {
	for _, locking := range []auction.Locking{auction.LockingPessimistic, auction.LockingOptimistic} {
		t.Run(string(locking), func(t *testing.T) {
			pool := server.NewDB(t, 10)
			m := metrics.New()
			api := httptest.NewServer(httpapi.NewRouter(httpapi.Options{
				Logger:         slog.New(slog.DiscardHandler),
				Auctions:       auction.NewService(pool, auction.WithLocking(locking), auction.WithObserver(m)),
				Ready:          pool.Ping,
				RequestTimeout: 5 * time.Second,
				Metrics:        m.HTTPMiddleware,
			}))
			defer api.Close()
			metricsSrv := httptest.NewServer(m.Handler())
			defer metricsSrv.Close()

			rep, err := Run(t.Context(), shortConfig("test-"+string(locking), api.URL, metricsSrv.URL), pool)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			for _, c := range rep.Checks {
				t.Logf("check %-52s passed=%v  %s", c.Name, c.Passed, c.Detail)
			}
			if failed := rep.Failed(); len(failed) > 0 {
				t.Fatalf("%d checks failed", len(failed))
			}
			t.Logf("throughput %.0f req/s, accepted %.1f/s, p50 %.2fms p99 %.2fms, results %v",
				rep.Throughput, rep.AcceptedPerSec, rep.Latency.P50, rep.Latency.P99, rep.Results)
			if rep.Latency.Count == 0 {
				t.Fatal("no latencies recorded in the measurement window")
			}
		})
	}
}

// The harness must catch a run that "measured nothing": an API that answers
// 201 to every bid without storing anything, and whose metrics never move.
// If these checks passed here, every benchmark number would be suspect.
func TestRunDetectsAStubThatMeasuresNothing(t *testing.T) {
	pool := server.NewDB(t, 4)
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id": 1}`))
	}))
	defer stub.Close()
	staticMetrics := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("http_requests_total{route=\"x\"} 5\n"))
	}))
	defer staticMetrics.Close()

	rep, err := Run(t.Context(), shortConfig("stub", stub.URL, staticMetrics.URL), pool)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	failed := map[string]bool{}
	for _, c := range rep.Failed() {
		failed[c.Name] = true
		t.Logf("failed as expected: %s: %s", c.Name, c.Detail)
	}
	for _, want := range []string{
		"server counted every response the client received",
		"client accepted count matches bids in the database",
		"load actually moved prices",
	} {
		if !failed[want] {
			t.Errorf("check %q passed against a stub that stores nothing", want)
		}
	}
}
