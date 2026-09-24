package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func testRouter(m *Metrics) http.Handler {
	r := chi.NewRouter()
	r.Use(m.HTTPMiddleware)
	r.Get("/auctions/{auctionID}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	r.Post("/auctions/{auctionID}/bids", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusConflict) })
	return r
}

func serve(t *testing.T, h http.Handler, method, path string) {
	t.Helper()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), method, path, nil))
}

func TestHTTPMiddlewareLabelsByRoutePattern(t *testing.T) {
	m := New()
	h := testRouter(m)

	serve(t, h, http.MethodGet, "/auctions/1")
	serve(t, h, http.MethodGet, "/auctions/2")
	serve(t, h, http.MethodPost, "/auctions/7/bids")
	serve(t, h, http.MethodGet, "/no/such/route")

	tests := []struct {
		method, route, status string
		want                  float64
	}{
		// Two different ids, one series: labelled by pattern, not path.
		{"GET", "/auctions/{auctionID}", "200", 2},
		{"POST", "/auctions/{auctionID}/bids", "409", 1},
		{"GET", unmatchedRoute, "404", 1},
	}
	for _, tt := range tests {
		got := testutil.ToFloat64(m.httpRequests.WithLabelValues(tt.method, tt.route, tt.status))
		if got != tt.want {
			t.Errorf("http_requests_total{%s %s %s} = %v, want %v", tt.method, tt.route, tt.status, got, tt.want)
		}
	}
	// No series may carry a raw id.
	if n := testutil.CollectAndCount(m.httpRequests); n != 3 {
		t.Errorf("http_requests_total has %d series, want 3 (raw paths leaking into labels?)", n)
	}
	if n := testutil.CollectAndCount(m.httpDuration); n != 3 {
		t.Errorf("http_request_duration_seconds has %d series, want 3", n)
	}
}

func TestHandlerExposesMetrics(t *testing.T) {
	m := New()
	serve(t, testRouter(m), http.MethodGet, "/auctions/1")

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))

	body := rec.Body.String()
	for _, want := range []string{
		`http_requests_total{method="GET",route="/auctions/{auctionID}",status="200"} 1`,
		"http_request_duration_seconds_bucket",
		"go_goroutines",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("exposition is missing %q", want)
		}
	}
}
