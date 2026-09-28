package metrics

import (
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/AryanSingh103/auction-engine/internal/breaker"
)

// unreachablePool connects lazily, so its scrape-time queries fail fast.
func unreachablePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(t.Context(), "postgres://nobody@127.0.0.1:1/none?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestRelayMetrics(t *testing.T) {
	r := NewWorkerRegistry()
	m := NewRelay(r, unreachablePool(t))
	m.Batch("published", 7, 10*time.Millisecond)
	m.Batch("published", 0, time.Millisecond)
	m.Batch("standby", 0, time.Millisecond)

	expected := `
# HELP outbox_relay_batches_total Relay batches: published (held the lock; may be empty), standby (another relay held it) or failed.
# TYPE outbox_relay_batches_total counter
outbox_relay_batches_total{result="failed"} 0
outbox_relay_batches_total{result="published"} 2
outbox_relay_batches_total{result="standby"} 1
# HELP outbox_relay_events_published_total Events published and marked (at least once: a crash before commit republishes).
# TYPE outbox_relay_events_published_total counter
outbox_relay_events_published_total 7
`
	if err := testutil.GatherAndCompare(r, strings.NewReader(expected), "outbox_relay_batches_total", "outbox_relay_events_published_total"); err != nil {
		t.Error(err)
	}
	// The backlog query fails: no backlog gauges (a gap, not a stale 0),
	// and the failure is counted.
	if n, err := testutil.GatherAndCount(r, "outbox_unpublished_events", "outbox_oldest_unpublished_age_seconds"); err != nil || n != 0 {
		t.Errorf("backlog gauges on a failed query: %d series (%v), want none", n, err)
	}
	lintClean(t, r)
}

func TestSettlerMetrics(t *testing.T) {
	r := NewWorkerRegistry()
	m := NewSettler(r, unreachablePool(t))
	m.Settled("paid")
	m.PaymentAttempt("unknown")
	m.PaymentAttempt("ok")
	m.DeadLettered()
	m.BreakerChanged(breaker.Closed, breaker.Open)

	expected := `
# HELP payment_breaker_state 1 for the payment circuit breaker's current state, 0 for the others.
# TYPE payment_breaker_state gauge
payment_breaker_state{state="closed"} 0
payment_breaker_state{state="half-open"} 0
payment_breaker_state{state="open"} 1
# HELP settlement_dead_letters_total Events sent to the dead-letter topic.
# TYPE settlement_dead_letters_total counter
settlement_dead_letters_total 1
`
	if err := testutil.GatherAndCompare(r, strings.NewReader(expected), "payment_breaker_state", "settlement_dead_letters_total"); err != nil {
		t.Error(err)
	}
	if got := testutil.ToFloat64(m.attempts.WithLabelValues("unknown")); got != 1 {
		t.Errorf("unknown attempts = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.settled.WithLabelValues("paid")); got != 1 {
		t.Errorf("paid = %v, want 1", got)
	}
	if n, err := testutil.GatherAndCount(r, "settlement_pending_invoices_scrape_errors_total"); err != nil || n != 1 {
		t.Errorf("scrape error counter: %d series (%v), want 1", n, err)
	}
	lintClean(t, r)
}

func lintClean(t *testing.T, r WorkerRegistry) {
	t.Helper()
	problems, err := testutil.GatherAndLint(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range problems {
		t.Errorf("lint: %s: %s", p.Metric, p.Text)
	}
}
