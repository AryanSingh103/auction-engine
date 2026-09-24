package invariants_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AryanSingh103/auction-engine/internal/invariants"
	"github.com/AryanSingh103/auction-engine/internal/testdb"
)

var server *testdb.Server

func TestMain(m *testing.M) {
	ctx := context.Background()
	var err error
	server, err = testdb.Start(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invariants tests: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	if err := server.Terminate(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "invariants tests: terminate: %v\n", err)
	}
	os.Exit(code)
}

// setup creates users 1-3, item 1 and auction 1 (open now, starting 1000,
// increment 100), then disables every trigger (including foreign-key
// triggers and the guards) so tests can plant rows the schema would reject.
// CHECK and UNIQUE constraints stay active.
func setup(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := server.NewDB(t, 2)
	exec(t, pool, `
		INSERT INTO users (name) VALUES ('u1'), ('u2'), ('u3');
		INSERT INTO items (title) VALUES ('unit');
		INSERT INTO auctions (item_id, start_at, end_at, starting_price, min_increment)
		VALUES (1, now() - interval '1 hour', now() + interval '1 hour', 1000, 100);
		ALTER TABLE bids DISABLE TRIGGER ALL;
		ALTER TABLE auctions DISABLE TRIGGER ALL;
		ALTER TABLE outbox DISABLE TRIGGER ALL;`)
	return pool
}

func exec(t *testing.T, pool *pgxpool.Pool, sql string) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), sql); err != nil {
		t.Fatalf("exec: %v\n%s", err, sql)
	}
}

// validBid is a correct first bid with its event and head, used as a base.
const validBid = `
	INSERT INTO bids (id, auction_id, user_id, amount, idempotency_key) OVERRIDING SYSTEM VALUE
	VALUES (1, 1, 1, 1000, 'k1');
	INSERT INTO outbox (auction_id, event_type, payload, bid_id) VALUES (1, 'bid_placed', '{}', 1);
	UPDATE auctions SET current_bid_id = 1, current_leader_id = 1, current_price = 1000 WHERE id = 1;`

func TestCleanDataHasNoViolations(t *testing.T) {
	pool := setup(t)
	exec(t, pool, validBid+`
		INSERT INTO bids (id, auction_id, user_id, amount, prev_bid_id, idempotency_key) OVERRIDING SYSTEM VALUE
		VALUES (2, 1, 2, 1100, 1, 'k2');
		INSERT INTO outbox (auction_id, event_type, payload, bid_id) VALUES (1, 'bid_placed', '{}', 2);
		UPDATE auctions SET current_bid_id = 2, current_leader_id = 2, current_price = 1100 WHERE id = 1;`)

	got, err := invariants.Run(t.Context(), pool)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("violations on valid data: %v", got)
	}
}

// Every check must detect a violation planted for it; a check that cannot
// fail proves nothing.
func TestEachCheckDetectsItsViolation(t *testing.T) {
	tests := []struct {
		check string
		plant string
	}{
		{"first bid meets starting price", `
			INSERT INTO bids (id, auction_id, user_id, amount, idempotency_key) OVERRIDING SYSTEM VALUE VALUES (1, 1, 1, 999, 'k');
			INSERT INTO outbox (auction_id, event_type, payload, bid_id) VALUES (1, 'bid_placed', '{}', 1);
			UPDATE auctions SET current_bid_id = 1, current_leader_id = 1, current_price = 1000 WHERE id = 1;`},
		{"each bid beats its predecessor by the increment", validBid + `
			INSERT INTO bids (id, auction_id, user_id, amount, prev_bid_id, idempotency_key) OVERRIDING SYSTEM VALUE VALUES (2, 1, 2, 1050, 1, 'k2');`},
		{"bids form a single chain", validBid + `
			INSERT INTO bids (id, auction_id, user_id, amount, prev_bid_id, idempotency_key) OVERRIDING SYSTEM VALUE VALUES (2, 1, 2, 5000, 99, 'k2');`},
		{"head is the highest bid", validBid + `
			UPDATE auctions SET current_price = 1500 WHERE id = 1;`},
		{"auctions without bids have no head", `
			UPDATE auctions SET current_bid_id = 42, current_leader_id = 1, current_price = 1000 WHERE id = 1;`},
		{"bids accepted inside the auction window", `
			INSERT INTO bids (id, auction_id, user_id, amount, idempotency_key, created_at) OVERRIDING SYSTEM VALUE
			VALUES (1, 1, 1, 1000, 'k', now() + interval '2 hours');`},
		{"nobody outbids themselves", validBid + `
			INSERT INTO bids (id, auction_id, user_id, amount, prev_bid_id, idempotency_key) OVERRIDING SYSTEM VALUE VALUES (2, 1, 1, 1100, 1, 'k2');`},
		{"every bid has exactly one outbox event", `
			INSERT INTO bids (id, auction_id, user_id, amount, idempotency_key) OVERRIDING SYSTEM VALUE VALUES (1, 1, 1, 1000, 'k');`},
		{"every bid event has its bid", `
			INSERT INTO outbox (auction_id, event_type, payload, bid_id) VALUES (1, 'bid_placed', '{}', 777);`},
	}

	// Guard against a check being added without a planted violation.
	covered := map[string]bool{}
	for _, tt := range tests {
		covered[tt.check] = true
	}
	for _, c := range invariants.Checks {
		if !covered[c.Name] {
			t.Errorf("check %q has no violation test", c.Name)
		}
	}

	for _, tt := range tests {
		t.Run(tt.check, func(t *testing.T) {
			pool := setup(t)
			exec(t, pool, tt.plant)

			got, err := invariants.Run(t.Context(), pool)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			for _, v := range got {
				if v.Check == tt.check {
					t.Logf("detected: %s", v)
					return
				}
			}
			t.Errorf("check %q did not report the planted violation; got %v", tt.check, got)
		})
	}
}
