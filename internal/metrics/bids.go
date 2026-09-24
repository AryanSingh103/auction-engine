package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/AryanSingh103/auction-engine/internal/auction"
)

// Compile-time check that Metrics can be passed to auction.WithObserver.
var _ auction.Observer = (*Metrics)(nil)

type bidMetrics struct {
	outcomes  *prometheus.CounterVec
	lockWait  prometheus.Histogram
	txTime    prometheus.Histogram
	conflicts prometheus.Counter
}

func newBidMetrics(reg *prometheus.Registry) bidMetrics {
	b := bidMetrics{
		outcomes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "bids_total",
			Help: "PlaceBid calls by outcome (accepted, replayed, too_low, ...).",
		}, []string{"outcome"}),
		lockWait: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "bid_lock_wait_seconds",
			Help: "Time to acquire the auction row lock, including one database round trip: the SELECT ... FOR UPDATE " +
				"(pessimistic) or the bid INSERT whose guard trigger takes the lock (optimistic). " +
				"Grows with the number of bids queued on the same auction.",
			Buckets: latencyBuckets,
		}),
		txTime: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "bid_transaction_seconds",
			Help:    "Duration of one bid transaction attempt, BEGIN to COMMIT or ROLLBACK.",
			Buckets: latencyBuckets,
		}),
		conflicts: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "bid_optimistic_conflicts_total",
			Help: "Optimistic bid attempts that found the auction head had moved. Each is retried " +
				"until the attempt limit; attempts exhausted show as bids_total{outcome=\"contention\"}.",
		}),
	}
	// Pre-create every outcome series at 0, so rate() and alerts see a value
	// from the first scrape instead of "no data".
	for _, o := range auction.AllOutcomes {
		b.outcomes.WithLabelValues(string(o))
	}
	reg.MustRegister(b.outcomes, b.lockWait, b.txTime, b.conflicts)
	return b
}

// BidOutcome implements auction.Observer.
func (m *Metrics) BidOutcome(o auction.Outcome) { m.bids.outcomes.WithLabelValues(string(o)).Inc() }

// LockWait implements auction.Observer.
func (m *Metrics) LockWait(d time.Duration) { m.bids.lockWait.Observe(d.Seconds()) }

// BidTransaction implements auction.Observer.
func (m *Metrics) BidTransaction(d time.Duration) { m.bids.txTime.Observe(d.Seconds()) }

// OptimisticConflict implements auction.Observer.
func (m *Metrics) OptimisticConflict() { m.bids.conflicts.Inc() }
