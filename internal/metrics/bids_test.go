package metrics

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/AryanSingh103/auction-engine/internal/auction"
)

func TestBidMetrics(t *testing.T) {
	m := New()

	// Every outcome exists at zero before anything happens.
	if n := testutil.CollectAndCount(m.bids.outcomes); n != len(auction.AllOutcomes) {
		t.Errorf("bids_total has %d series before any bid, want %d", n, len(auction.AllOutcomes))
	}

	m.BidOutcome(auction.OutcomeAccepted)
	m.BidOutcome(auction.OutcomeAccepted)
	m.BidOutcome(auction.OutcomeTooLow)
	m.LockWait(3 * time.Millisecond)
	m.BidTransaction(5 * time.Millisecond)

	if got := testutil.ToFloat64(m.bids.outcomes.WithLabelValues("accepted")); got != 2 {
		t.Errorf("bids_total{accepted} = %v, want 2", got)
	}
	if got := testutil.ToFloat64(m.bids.outcomes.WithLabelValues("too_low")); got != 1 {
		t.Errorf("bids_total{too_low} = %v, want 1", got)
	}
	if n := testutil.CollectAndCount(m.bids.lockWait); n != 1 {
		t.Errorf("bid_lock_wait_seconds series = %d, want 1", n)
	}
	problems, err := testutil.GatherAndLint(m.Registry(), "bids_total", "bid_lock_wait_seconds", "bid_transaction_seconds")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range problems {
		t.Errorf("lint: %s: %s", p.Metric, p.Text)
	}
}
