// Package metrics defines the process's Prometheus metrics and exposes them
// on a registry of its own (not the global default registry, so tests can
// create independent instances and nothing registers metrics as a side
// effect of importing a package). See docs/decisions/013.
package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// latencyBuckets span 0.5ms to 10s in 16 exponential steps. A bid on an
// uncontended auction takes around a millisecond, so the default buckets
// (starting at 5ms) would put almost everything in the first bucket and
// hide the distribution that matters.
var latencyBuckets = prometheus.ExponentialBucketsRange(0.0005, 10, 16)

// Metrics holds every metric the API exports.
type Metrics struct {
	registry *prometheus.Registry

	httpRequests *prometheus.CounterVec
	httpDuration *prometheus.HistogramVec
}

// New creates the metrics and registers them, plus the Go runtime and
// process collectors, on a fresh registry.
func New() *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),
		httpRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "HTTP requests served, by method, route pattern and status code.",
		}, []string{"method", "route", "status"}),
		httpDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "Time from receiving a request to finishing its response, by method and route pattern.",
			Buckets: latencyBuckets,
		}, []string{"method", "route"}),
	}
	m.registry.MustRegister(
		m.httpRequests,
		m.httpDuration,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return m
}

// Handler serves the metrics in the Prometheus exposition format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// Registry exposes the registry so other packages' collectors can register.
func (m *Metrics) Registry() *prometheus.Registry { return m.registry }

// unmatchedRoute labels requests that matched no route (404s). Using the raw
// path instead would let any client create unbounded label values, which
// grows Prometheus memory without limit (a "cardinality explosion").
const unmatchedRoute = "unmatched"

// HTTPMiddleware records a count and a duration for every request, labelled
// with chi's route pattern (e.g. /auctions/{auctionID}/bids), never the raw
// path. It must be registered on the chi router: the pattern is only known
// once routing has finished, so it is read after the handler returns.
func (m *Metrics) HTTPMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)

		next.ServeHTTP(ww, r)

		route := unmatchedRoute
		if rctx := chi.RouteContext(r.Context()); rctx != nil {
			if p := rctx.RoutePattern(); p != "" {
				route = p
			}
		}
		status := ww.Status()
		if status == 0 {
			status = http.StatusOK // nothing written: net/http sends 200
		}
		m.httpRequests.WithLabelValues(r.Method, route, strconv.Itoa(status)).Inc()
		m.httpDuration.WithLabelValues(r.Method, route).Observe(time.Since(start).Seconds())
	})
}
