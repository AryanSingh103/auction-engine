package metrics

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/AryanSingh103/auction-engine/internal/breaker"
)

// scrapeQueryTimeout bounds the database queries run at scrape time.
const scrapeQueryTimeout = 2 * time.Second

// WorkerRegistry is a registry for a background process (relay, settler),
// with the Go and process collectors, and its /metrics handler.
type WorkerRegistry struct {
	*prometheus.Registry
}

// NewWorkerRegistry returns a registry with the standard collectors.
func NewWorkerRegistry() WorkerRegistry {
	r := prometheus.NewRegistry()
	r.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return WorkerRegistry{r}
}

// Handler serves the registry's metrics.
func (r WorkerRegistry) Handler() http.Handler {
	return promhttp.HandlerFor(r.Registry, promhttp.HandlerOpts{})
}

// StartServer serves h on addr in the background. It binds before
// returning, so a busy port fails startup instead of being logged later.
// stop shuts the server down, waiting at most timeout for scrapes in flight.
func StartServer(ctx context.Context, addr string, h http.Handler, timeout time.Duration) (stop func(), err error) {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen on %q: %w", addr, err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: timeout, WriteTimeout: timeout}
	go func() { _ = srv.Serve(ln) }()
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}, nil
}

// Relay implements outbox.Observer.
type Relay struct {
	batches  *prometheus.CounterVec
	events   prometheus.Counter
	duration *prometheus.HistogramVec
}

// NewRelay registers the relay's metrics, including the outbox backlog,
// read from pool at scrape time.
func NewRelay(r WorkerRegistry, pool *pgxpool.Pool) *Relay {
	m := &Relay{
		batches: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "outbox_relay_batches_total",
			Help: "Relay batches: published (held the lock; may be empty), standby (another relay held it) or failed.",
		}, []string{"result"}),
		events: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "outbox_relay_events_published_total",
			Help: "Events published and marked (at least once: a crash before commit republishes).",
		}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "outbox_relay_batch_duration_seconds",
			Help:    "Duration of relay batches by result.",
			Buckets: prometheus.ExponentialBuckets(0.001, 2, 14), // 1ms .. ~8s
		}, []string{"result"}),
	}
	for _, res := range []string{"published", "standby", "failed"} {
		m.batches.WithLabelValues(res)
	}
	r.MustRegister(m.batches, m.events, m.duration, newQueryGauges(pool, "outbox_backlog",
		`SELECT count(*)::float8, coalesce(extract(epoch FROM clock_timestamp() - min(created_at)), 0)::float8
		 FROM outbox WHERE published_at IS NULL`,
		gaugeSpec{"outbox_unpublished_events", "Outbox events not yet published (uses the partial index)."},
		gaugeSpec{"outbox_oldest_unpublished_age_seconds", "Age of the oldest unpublished event; the relay's alert signal."},
	))
	return m
}

// Batch implements outbox.Observer.
func (m *Relay) Batch(result string, n int, d time.Duration) {
	m.batches.WithLabelValues(result).Inc()
	m.events.Add(float64(n))
	m.duration.WithLabelValues(result).Observe(d.Seconds())
}

// Settler implements settlement.Observer and reports breaker state.
type Settler struct {
	settled  *prometheus.CounterVec
	attempts *prometheus.CounterVec
	dead     prometheus.Counter
	breaker  *prometheus.GaugeVec
}

// NewSettler registers the settler's metrics, including pending invoices,
// read from pool at scrape time.
func NewSettler(r WorkerRegistry, pool *pgxpool.Pool) *Settler {
	m := &Settler{
		settled: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "settlement_results_total",
			Help: "Settle calls by result: paid, declined, already_settled, no_winner, gave_up (dead-lettered, invoice pending), rejected, error.",
		}, []string{"result"}),
		attempts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "payment_attempts_total",
			Help: "Calls to the payment provider by result: ok, declined, rejected, unknown (5xx, timeout, connection error).",
		}, []string{"result"}),
		dead: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "settlement_dead_letters_total",
			Help: "Events sent to the dead-letter topic.",
		}),
		breaker: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "payment_breaker_state",
			Help: "1 for the payment circuit breaker's current state, 0 for the others.",
		}, []string{"state"}),
	}
	for _, res := range []string{"paid", "declined", "already_settled", "no_winner", "gave_up", "rejected", "error"} {
		m.settled.WithLabelValues(res)
	}
	for _, res := range []string{"ok", "declined", "rejected", "unknown"} {
		m.attempts.WithLabelValues(res)
	}
	m.BreakerChanged(breaker.Closed, breaker.Closed)
	r.MustRegister(m.settled, m.attempts, m.dead, m.breaker, newQueryGauges(pool, "settlement_pending_invoices",
		`SELECT count(*)::float8, coalesce(extract(epoch FROM clock_timestamp() - min(created_at)), 0)::float8
		 FROM invoices WHERE status = 'pending'`,
		gaugeSpec{"settlement_pending_invoices", "Invoices not yet paid or declined."},
		gaugeSpec{"settlement_oldest_pending_invoice_age_seconds", "Age of the oldest pending invoice."},
	))
	return m
}

// Settled implements settlement.Observer.
func (m *Settler) Settled(result string) { m.settled.WithLabelValues(result).Inc() }

// PaymentAttempt implements settlement.Observer.
func (m *Settler) PaymentAttempt(result string) { m.attempts.WithLabelValues(result).Inc() }

// DeadLettered implements settlement.Observer.
func (m *Settler) DeadLettered() { m.dead.Inc() }

// BreakerChanged is the breaker's onChange callback.
func (m *Settler) BreakerChanged(_, to breaker.State) {
	for _, s := range []breaker.State{breaker.Closed, breaker.Open, breaker.HalfOpen} {
		v := 0.0
		if s == to {
			v = 1
		}
		m.breaker.WithLabelValues(s.String()).Set(v)
	}
}

type gaugeSpec struct{ name, help string }

// queryGauges exports gauges computed by one query at scrape time. If the
// query fails, the gauges are left out of that scrape (a gap, not a stale
// number) and the error counter goes up.
type queryGauges struct {
	pool   *pgxpool.Pool
	sql    string
	descs  []*prometheus.Desc
	errors prometheus.Counter
}

func newQueryGauges(pool *pgxpool.Pool, name, sql string, specs ...gaugeSpec) *queryGauges {
	q := &queryGauges{pool: pool, sql: sql, errors: prometheus.NewCounter(prometheus.CounterOpts{
		Name: name + "_scrape_errors_total",
		Help: "Scrapes where the " + name + " query failed.",
	})}
	for _, s := range specs {
		q.descs = append(q.descs, prometheus.NewDesc(s.name, s.help, nil, nil))
	}
	return q
}

func (q *queryGauges) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range q.descs {
		ch <- d
	}
	q.errors.Describe(ch)
}

func (q *queryGauges) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), scrapeQueryTimeout)
	defer cancel()
	values := make([]float64, len(q.descs))
	dst := make([]any, len(values))
	for i := range values {
		dst[i] = &values[i]
	}
	if err := q.pool.QueryRow(ctx, q.sql).Scan(dst...); err != nil {
		q.errors.Inc()
	} else {
		for i, d := range q.descs {
			ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, values[i])
		}
	}
	q.errors.Collect(ch)
}
