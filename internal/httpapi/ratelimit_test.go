package httpapi

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/AryanSingh103/auction-engine/internal/auction"
	"github.com/AryanSingh103/auction-engine/internal/ratelimit"
)

type recorded struct {
	mu      sync.Mutex
	results map[string]int
}

func (r *recorded) record(result string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.results == nil {
		r.results = map[string]int{}
	}
	r.results[result]++
}

func limitedRouter(t *testing.T, limiter BidLimiter, rec *recorded) http.Handler {
	t.Helper()
	pool := server.NewDB(t, 4)
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO users (name) VALUES ('alice'), ('bob');
		INSERT INTO items (title) VALUES ('unit');
		INSERT INTO auctions (item_id, start_at, end_at, starting_price, min_increment)
		VALUES (1, clock_timestamp() - interval '1 minute', clock_timestamp() + interval '1 hour', 1000, 100);`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return NewRouter(Options{
		Logger: discardLogger, Auctions: auction.NewService(pool), Ready: pool.Ping,
		RequestTimeout: 5 * time.Second, BidLimiter: limiter, RecordRateLimit: rec.record,
	})
}

func TestBidRateLimit(t *testing.T) {
	rec := &recorded{}
	// Burst 2, and a slow refill so the test cannot race it.
	h := limitedRouter(t, ratelimit.New(redisServer.NewClient(t), "rl:", 0.5, 2), rec)

	// Two bids pass the limiter (the second is rejected by bid rules, which
	// still consumes a token: the limiter protects capacity, whatever the
	// outcome).
	for i := range 2 {
		rec1 := (bidCall{user: "1", key: fmt.Sprintf("k%d", i), body: `{"amount": 1000}`}).do(t, h)
		if rec1.Code == http.StatusTooManyRequests {
			t.Fatalf("bid %d within burst was limited", i+1)
		}
	}
	limited := (bidCall{user: "1", key: "k3", body: `{"amount": 5000}`}).do(t, h)
	if limited.Code != http.StatusTooManyRequests {
		t.Fatalf("third bid: status %d, want 429; body %s", limited.Code, limited.Body)
	}
	if code, _ := errorCode(t, limited); code != "rate_limited" {
		t.Errorf("error code = %q, want rate_limited", code)
	}
	secs, err := strconv.Atoi(limited.Header().Get("Retry-After"))
	if err != nil || secs < 1 || secs > 2 {
		t.Errorf("Retry-After = %q, want 1 or 2 seconds (refill 0.5/s)", limited.Header().Get("Retry-After"))
	}

	// Another user has their own bucket.
	if other := (bidCall{user: "2", key: "b1", body: `{"amount": 5000}`}).do(t, h); other.Code == http.StatusTooManyRequests {
		t.Error("a different user was limited by user 1's bucket")
	}
	// Reads are never limited.
	get := httptest.NewRecorder()
	h.ServeHTTP(get, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/auctions/1", nil))
	if get.Code != http.StatusOK {
		t.Errorf("GET status %d, want 200", get.Code)
	}
	if rec.results["limited"] != 1 || rec.results["allowed"] != 3 {
		t.Errorf("recorded decisions %v, want 3 allowed and 1 limited", rec.results)
	}
}

// With Redis unreachable, bids must still be served (fail open), and the
// failure must be visible in metrics.
func TestBidRateLimitFailsOpen(t *testing.T) {
	rec := &recorded{}
	c := redisServer.NewClient(t)
	_ = c.Close()
	h := limitedRouter(t, ratelimit.New(c, "rl:", 0.5, 1), rec)

	for i := range 3 {
		res := (bidCall{user: "1", key: fmt.Sprintf("k%d", i), body: fmt.Sprintf(`{"amount": %d}`, 1000+i*100)}).do(t, h)
		if res.Code == http.StatusTooManyRequests || res.Code >= 500 {
			t.Fatalf("bid %d with Redis down: status %d; want it served", i+1, res.Code)
		}
	}
	if rec.results["error"] != 3 {
		t.Errorf("recorded decisions %v, want 3 errors", rec.results)
	}
}
