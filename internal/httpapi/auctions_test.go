package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AryanSingh103/auction-engine/internal/auction"
	"github.com/AryanSingh103/auction-engine/internal/testdb"
)

var server *testdb.Server

func TestMain(m *testing.M) {
	ctx := context.Background()
	var err error
	server, err = testdb.Start(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "httpapi tests: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	if err := server.Terminate(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "httpapi tests: terminate: %v\n", err)
	}
	os.Exit(code)
}

// apiFixture is a router over a fresh database with users 1 and 2 and
// auction 1 (open, starting price 1000, increment 100).
func apiFixture(t *testing.T, requestTimeout time.Duration) (http.Handler, *pgxpool.Pool) {
	t.Helper()
	pool := server.NewDB(t, 4)
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO users (name) VALUES ('alice'), ('bob');
		INSERT INTO items (title) VALUES ('unit 7');
		INSERT INTO auctions (item_id, start_at, end_at, starting_price, min_increment)
		VALUES (1, clock_timestamp() - interval '1 minute', clock_timestamp() + interval '1 hour', 1000, 100);`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	h := NewRouter(Options{
		Logger:         discardLogger,
		Auctions:       auction.NewService(pool),
		Ready:          pool.Ping,
		RequestTimeout: requestTimeout,
	})
	return h, pool
}

type bidCall struct {
	path, user, key, body string
}

func (c bidCall) do(t *testing.T, h http.Handler) *httptest.ResponseRecorder {
	t.Helper()
	path := c.path
	if path == "" {
		path = "/auctions/1/bids"
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, strings.NewReader(c.body))
	if c.user != "" {
		req.Header.Set("X-User-ID", c.user)
	}
	if c.key != "" {
		req.Header.Set("Idempotency-Key", c.key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) (string, *int64) {
	t.Helper()
	var body errorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body is not JSON: %v\n%s", err, rec.Body)
	}
	return body.Error.Code, body.Error.Minimum
}

func TestPlaceBidResponses(t *testing.T) {
	h, _ := apiFixture(t, 5*time.Second)
	// Alice leads at 1000 before the table runs.
	if rec := (bidCall{user: "1", key: "alice-1", body: `{"amount": 1000}`}).do(t, h); rec.Code != http.StatusCreated {
		t.Fatalf("setup bid: %d %s", rec.Code, rec.Body)
	}

	tests := []struct {
		name     string
		call     bidCall
		status   int
		code     string // expected error code; "" for success
		replayed bool
	}{
		{"replay of the setup bid", bidCall{user: "1", key: "alice-1", body: `{"amount": 1000}`}, http.StatusOK, "", true},
		{"key reused with a different amount", bidCall{user: "1", key: "alice-1", body: `{"amount": 2000}`}, http.StatusUnprocessableEntity, "idempotency_key_reused", false},
		{"too low", bidCall{user: "2", key: "bob-1", body: `{"amount": 1099}`}, http.StatusConflict, "bid_too_low", false},
		{"leader outbids self", bidCall{user: "1", key: "alice-2", body: `{"amount": 5000}`}, http.StatusConflict, "self_outbid", false},
		{"missing user header", bidCall{key: "k", body: `{"amount": 5000}`}, http.StatusUnauthorized, "missing_user", false},
		{"non-numeric user", bidCall{user: "bob", key: "k", body: `{"amount": 5000}`}, http.StatusUnauthorized, "missing_user", false},
		{"unknown user", bidCall{user: "99", key: "k", body: `{"amount": 5000}`}, http.StatusUnauthorized, "unknown_user", false},
		{"missing idempotency key", bidCall{user: "2", body: `{"amount": 5000}`}, http.StatusBadRequest, "invalid_idempotency_key", false},
		{"idempotency key too long", bidCall{user: "2", key: strings.Repeat("k", 256), body: `{"amount": 5000}`}, http.StatusBadRequest, "invalid_idempotency_key", false},
		{"malformed JSON", bidCall{user: "2", key: "k", body: `{"amount": `}, http.StatusBadRequest, "invalid_body", false},
		{"unknown field", bidCall{user: "2", key: "k", body: `{"amount": 5000, "currency": "USD"}`}, http.StatusBadRequest, "invalid_body", false},
		{"missing amount", bidCall{user: "2", key: "k", body: `{}`}, http.StatusBadRequest, "invalid_body", false},
		{"amount as string", bidCall{user: "2", key: "k", body: `{"amount": "5000"}`}, http.StatusBadRequest, "invalid_body", false},
		{"fractional amount", bidCall{user: "2", key: "k", body: `{"amount": 50.5}`}, http.StatusBadRequest, "invalid_body", false},
		{"two JSON objects", bidCall{user: "2", key: "k", body: `{"amount": 5000}{"amount": 1}`}, http.StatusBadRequest, "invalid_body", false},
		{"body over 1KB", bidCall{user: "2", key: "k", body: `{"amount": 5000` + strings.Repeat(" ", 2048) + `}`}, http.StatusBadRequest, "invalid_body", false},
		{"zero amount", bidCall{user: "2", key: "k", body: `{"amount": 0}`}, http.StatusBadRequest, "invalid_amount", false},
		{"unknown auction", bidCall{path: "/auctions/999/bids", user: "2", key: "k", body: `{"amount": 5000}`}, http.StatusNotFound, "auction_not_found", false},
		{"non-numeric auction id", bidCall{path: "/auctions/abc/bids", user: "2", key: "k", body: `{"amount": 5000}`}, http.StatusNotFound, "auction_not_found", false},
		{"valid outbid", bidCall{user: "2", key: "bob-2", body: `{"amount": 1100}`}, http.StatusCreated, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := tt.call.do(t, h)
			if rec.Code != tt.status {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, tt.status, rec.Body)
			}
			if got := rec.Header().Get("Idempotent-Replayed") == "true"; got != tt.replayed {
				t.Errorf("Idempotent-Replayed = %v, want %v", got, tt.replayed)
			}
			if tt.code == "" {
				var bid auction.Bid
				if err := json.Unmarshal(rec.Body.Bytes(), &bid); err != nil || bid.ID == 0 {
					t.Errorf("success body is not a bid: %v %s", err, rec.Body)
				}
				return
			}
			if code, _ := errorCode(t, rec); code != tt.code {
				t.Errorf("error code = %q, want %q; body %s", code, tt.code, rec.Body)
			}
		})
	}
}

func TestBidTooLowReportsMinimum(t *testing.T) {
	h, _ := apiFixture(t, 5*time.Second)
	rec := (bidCall{user: "1", key: "k", body: `{"amount": 500}`}).do(t, h)
	code, minimum := errorCode(t, rec)
	if code != "bid_too_low" || minimum == nil || *minimum != 1000 {
		t.Errorf("code %q minimum %v, want bid_too_low with minimum 1000; body %s", code, minimum, rec.Body)
	}
}

// With a request deadline that has already passed when the handler runs, the
// database call fails with context.DeadlineExceeded and the client must get
// a 503 (retry-safe), not a 500. Here the deadline passes before the
// transaction starts, so nothing may be written. (A deadline that fires
// during COMMIT leaves the outcome unknown; the idempotency key covers it.)
func TestPlaceBidTimeoutIs503(t *testing.T) {
	h, pool := apiFixture(t, time.Nanosecond)
	rec := (bidCall{user: "1", key: "k", body: `{"amount": 1000}`}).do(t, h)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body %s", rec.Code, rec.Body)
	}
	if code, _ := errorCode(t, rec); code != "timeout" {
		t.Errorf("code = %q, want timeout", code)
	}
	var n int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM bids`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("%d bids written by a timed-out request, want 0", n)
	}
}

func TestGetAuction(t *testing.T) {
	h, _ := apiFixture(t, 5*time.Second)
	if rec := (bidCall{user: "2", key: "k", body: `{"amount": 1200}`}).do(t, h); rec.Code != http.StatusCreated {
		t.Fatalf("setup bid: %d %s", rec.Code, rec.Body)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/auctions/1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
	}
	var got auctionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.CurrentPrice == nil || *got.CurrentPrice != 1200 || got.CurrentLeaderID == nil || *got.CurrentLeaderID != 2 || got.MinimumBid != 1300 {
		t.Errorf("auction = %+v, want price 1200, leader 2, minimum 1300", got)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/auctions/42", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("missing auction status = %d, want 404", rec.Code)
	}
}
