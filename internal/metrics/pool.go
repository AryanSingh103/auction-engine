package metrics

import (
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
)

// poolCollector reports pgxpool statistics. It implements
// prometheus.Collector and reads pool.Stat() at scrape time, so the values
// are always current and nothing polls in the background.
type poolCollector struct {
	pool *pgxpool.Pool

	acquired, idle, total, max                        *prometheus.Desc
	acquires, acquireSeconds, emptyAcquires, canceled *prometheus.Desc
	emptyWaitSeconds                                  *prometheus.Desc
}

// RegisterPool adds pool statistics to the metrics. The most telling series
// under load is db_pool_empty_acquires_total: requests that found no idle
// connection and had to wait for one, i.e. the pool, not Postgres, was the
// bottleneck at that moment.
func (m *Metrics) RegisterPool(pool *pgxpool.Pool) {
	desc := func(name, help string) *prometheus.Desc {
		return prometheus.NewDesc("db_pool_"+name, help, nil, nil)
	}
	m.registry.MustRegister(&poolCollector{
		pool:             pool,
		acquired:         desc("acquired_conns", "Connections currently checked out of the pool."),
		idle:             desc("idle_conns", "Connections currently idle in the pool."),
		total:            desc("total_conns", "Connections currently open (acquired + idle + being constructed)."),
		max:              desc("max_conns", "Configured maximum pool size (DB_MAX_CONNS)."),
		acquires:         desc("acquires_total", "Successful connection acquisitions."),
		acquireSeconds:   desc("acquire_duration_seconds_total", "Total time spent acquiring connections."),
		emptyAcquires:    desc("empty_acquires_total", "Acquisitions that had to wait because no connection was idle."),
		emptyWaitSeconds: desc("empty_acquire_wait_seconds_total", "Total time spent waiting in acquisitions that found the pool empty."),
		canceled:         desc("canceled_acquires_total", "Acquisitions abandoned because their context was cancelled."),
	})
}

func (c *poolCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{c.acquired, c.idle, c.total, c.max, c.acquires, c.acquireSeconds, c.emptyAcquires, c.emptyWaitSeconds, c.canceled} {
		ch <- d
	}
}

func (c *poolCollector) Collect(ch chan<- prometheus.Metric) {
	s := c.pool.Stat()
	gauge := func(d *prometheus.Desc, v float64) { ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v) }
	counter := func(d *prometheus.Desc, v float64) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.CounterValue, v)
	}

	gauge(c.acquired, float64(s.AcquiredConns()))
	gauge(c.idle, float64(s.IdleConns()))
	gauge(c.total, float64(s.TotalConns()))
	gauge(c.max, float64(s.MaxConns()))
	counter(c.acquires, float64(s.AcquireCount()))
	counter(c.acquireSeconds, s.AcquireDuration().Seconds())
	counter(c.emptyAcquires, float64(s.EmptyAcquireCount()))
	counter(c.emptyWaitSeconds, s.EmptyAcquireWaitTime().Seconds())
	counter(c.canceled, float64(s.CanceledAcquireCount()))
}
