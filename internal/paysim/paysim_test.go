package paysim_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AryanSingh103/auction-engine/internal/paysim"
	"github.com/AryanSingh103/auction-engine/internal/testdb"
)

var db *testdb.Server

func TestMain(m *testing.M) {
	ctx := context.Background()
	var err error
	if db, err = testdb.Start(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "paysim tests: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	_ = db.Terminate(ctx)
	os.Exit(code)
}

func newStore(t *testing.T) (*paysim.Store, *pgxpool.Pool) {
	t.Helper()
	pool := db.NewDB(t, 20)
	s := paysim.NewStore(pool)
	if err := s.Migrate(t.Context()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return s, pool
}

func charges(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM paysim.charges`).Scan(&n); err != nil {
		t.Fatalf("count charges: %v", err)
	}
	return n
}

func TestConcurrentChargesWithOneKeyChargeOnce(t *testing.T) {
	s, pool := newStore(t)
	const calls = 50
	ids := make([]string, calls)
	replays := make([]bool, calls)
	var wg sync.WaitGroup
	for i := range calls {
		wg.Go(func() {
			c, replayed, err := s.Charge(t.Context(), "invoice-1", 1100, 7)
			if err != nil {
				t.Errorf("charge: %v", err)
			}
			ids[i], replays[i] = c.ID, replayed
		})
	}
	wg.Wait()
	fresh := 0
	for i := range calls {
		if ids[i] != ids[0] {
			t.Fatalf("call %d got charge %s, call 0 got %s", i, ids[i], ids[0])
		}
		if !replays[i] {
			fresh++
		}
	}
	if fresh != 1 {
		t.Errorf("%d calls report a fresh charge, want 1", fresh)
	}
	if n := charges(t, pool); n != 1 {
		t.Errorf("%d charges stored, want 1", n)
	}
}

func TestKeyReuseWithDifferentRequestIsRefused(t *testing.T) {
	s, _ := newStore(t)
	if _, _, err := s.Charge(t.Context(), "k", 1100, 7); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ amount, customer int64 }{{1200, 7}, {1100, 8}} {
		if _, _, err := s.Charge(t.Context(), "k", tc.amount, tc.customer); !errors.Is(err, paysim.ErrKeyReused) {
			t.Errorf("charge(%d, %d) under a used key: %v, want ErrKeyReused", tc.amount, tc.customer, err)
		}
	}
}

// server runs the handler with a fixed roll.
func server(t *testing.T, store *paysim.Store, roll float64, hang time.Duration) *httptest.Server {
	t.Helper()
	faults := paysim.Faults{FailureRate: 0.1, HangRate: 0.1, Hang: hang, Roll: func() float64 { return roll }}
	srv := httptest.NewServer(paysim.NewHandler(store, faults, slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(srv.Close)
	return srv
}

func post(t *testing.T, ctx context.Context, url, key string, amount int64) (int, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]int64{"amount": amount, "customer_id": 7})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url+"/charges", bytes.NewReader(body))
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, map[string]any{"transport_error": err.Error()}
	}
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestFaults(t *testing.T) {
	ctx := t.Context()

	t.Run("fail before charging", func(t *testing.T) {
		s, pool := newStore(t)
		if code, _ := post(t, ctx, server(t, s, 0.0, 0).URL, "k", 1100); code != http.StatusServiceUnavailable {
			t.Errorf("status %d, want 503", code)
		}
		if n := charges(t, pool); n != 0 {
			t.Errorf("%d charges after a pre-charge failure, want 0", n)
		}
	})

	t.Run("response lost after charging, then retried", func(t *testing.T) {
		s, pool := newStore(t)
		if code, _ := post(t, ctx, server(t, s, 0.07, 0).URL, "k", 1100); code != http.StatusInternalServerError {
			t.Fatalf("status %d, want 500", code)
		}
		if n := charges(t, pool); n != 1 {
			t.Fatalf("%d charges after a lost response, want 1 (it charged)", n)
		}
		code, body := post(t, ctx, server(t, s, 0.99, 0).URL, "k", 1100)
		if code != http.StatusOK || body["replayed"] != true {
			t.Fatalf("retry = %d %v, want 200 replayed", code, body)
		}
		if n := charges(t, pool); n != 1 {
			t.Errorf("%d charges after the retry, want still 1", n)
		}
	})

	t.Run("hang after charging", func(t *testing.T) {
		s, pool := newStore(t)
		url := server(t, s, 0.15, 300*time.Millisecond).URL
		start := time.Now()
		if code, _ := post(t, ctx, url, "k", 1100); code != http.StatusOK || time.Since(start) < 300*time.Millisecond {
			t.Errorf("status %d after %s, want 200 after at least 300ms", code, time.Since(start))
		}
		// A client that gives up first has still been charged.
		short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		defer cancel()
		if code, _ := post(t, short, url, "k2", 1200); code != 0 {
			t.Errorf("status %d, want a client timeout", code)
		}
		// The handler charges before it hangs, but the client may give up
		// before that commit lands, so wait for it (bounded).
		deadline := time.Now().Add(5 * time.Second)
		for charges(t, pool) < 2 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if n := charges(t, pool); n != 2 {
			t.Errorf("%d charges, want 2 (the timed-out one charged too)", n)
		}
	})

	t.Run("declined", func(t *testing.T) {
		s, pool := newStore(t)
		if code, _ := post(t, ctx, server(t, s, 0.99, 0).URL, "k", 1113); code != http.StatusPaymentRequired {
			t.Errorf("status %d, want 402", code)
		}
		if n := charges(t, pool); n != 0 {
			t.Errorf("%d charges after a decline, want 0", n)
		}
	})

	t.Run("bad requests", func(t *testing.T) {
		s, _ := newStore(t)
		url := server(t, s, 0.99, 0).URL
		if code, _ := post(t, ctx, url, "", 1100); code != http.StatusBadRequest {
			t.Errorf("no key: status %d, want 400", code)
		}
		if code, _ := post(t, ctx, url, "k", 0); code != http.StatusBadRequest {
			t.Errorf("zero amount: status %d, want 400", code)
		}
		if code, _ := post(t, ctx, url, "k", 1100); code != http.StatusOK {
			t.Fatalf("first: status %d", code)
		}
		if code, _ := post(t, ctx, url, "k", 1200); code != http.StatusUnprocessableEntity {
			t.Errorf("key reuse: status %d, want 422", code)
		}
	})
}
