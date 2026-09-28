package migrations_test

// Tests for the invariants the schema enforces on its own, independent of
// any Go bid-path code: raw SQL goes in, and the database must accept or
// reject it with a specific SQLSTATE and constraint. See docs/decisions/008.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AryanSingh103/auction-engine/internal/testdb"
)

var server *testdb.Server

func TestMain(m *testing.M) {
	ctx := context.Background()
	var err error
	server, err = testdb.Start(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "schema tests: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	if err := server.Terminate(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "schema tests: terminate: %v\n", err)
	}
	os.Exit(code)
}

// fixture is one test database with three users, one item and one auction
// that is open now, starting at 1000 with an increment of 100.
type fixture struct {
	pool    *pgxpool.Pool
	users   [3]int64
	item    int64
	auction int64
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := t.Context()
	f := &fixture{pool: server.NewDB(t, 4)}
	for i := range f.users {
		mustScan(t, f.pool.QueryRow(ctx, `INSERT INTO users (name) VALUES ($1) RETURNING id`, fmt.Sprintf("u%d", i)), &f.users[i])
	}
	mustScan(t, f.pool.QueryRow(ctx, `INSERT INTO items (title) VALUES ('unit') RETURNING id`), &f.item)
	f.auction = f.newAuction(t, "clock_timestamp() - interval '1 minute'", "clock_timestamp() + interval '1 hour'")
	return f
}

// newAuction inserts an auction with SQL expressions for its window.
func (f *fixture) newAuction(t *testing.T, startExpr, endExpr string) int64 {
	t.Helper()
	var id int64
	mustScan(t, f.pool.QueryRow(t.Context(), fmt.Sprintf(`
		INSERT INTO auctions (item_id, start_at, end_at, starting_price, min_increment)
		VALUES ($1, %s, %s, 1000, 100) RETURNING id`, startExpr, endExpr), f.item), &id)
	return id
}

// placeBid does what a correct bid path does, in one transaction: insert the
// bid, its outbox event, and advance the auction head. It returns the new
// bid's id or the database error.
func (f *fixture) placeBid(ctx context.Context, auction, user, amount int64, prev *int64, key string) (int64, error) {
	var id int64
	err := pgx.BeginFunc(ctx, f.pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			INSERT INTO bids (auction_id, user_id, amount, prev_bid_id, idempotency_key)
			VALUES ($1, $2, $3, $4, $5) RETURNING id`,
			auction, user, amount, prev, key).Scan(&id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO outbox (auction_id, event_type, payload, bid_id)
			VALUES ($1, 'bid_placed', '{}', $2)`, auction, id); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			UPDATE auctions SET current_price = $2, current_leader_id = $3, current_bid_id = $4
			WHERE id = $1`, auction, amount, user, id)
		return err
	})
	return id, err
}

// closeAuction does what a correct close does, in one transaction: set
// the status and write the auction_closed event.
func (f *fixture) closeAuction(ctx context.Context, auction int64) error {
	return pgx.BeginFunc(ctx, f.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE auctions SET status = 'closed' WHERE id = $1`, auction); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO outbox (auction_id, event_type, payload) VALUES ($1, 'auction_closed', '{}')`, auction)
		return err
	})
}

func mustScan(t *testing.T, row pgx.Row, dst ...any) {
	t.Helper()
	if err := row.Scan(dst...); err != nil {
		t.Fatalf("setup query: %v", err)
	}
}

// wantPgError asserts err is a Postgres error with the given SQLSTATE and,
// if constraint is non-empty, the given constraint name.
func wantPgError(t *testing.T, err error, code, constraint string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("error = %v, want Postgres error %s", err, code)
	}
	if pgErr.Code != code {
		t.Fatalf("SQLSTATE = %s (%s), want %s", pgErr.Code, pgErr.Message, code)
	}
	if constraint != "" && pgErr.ConstraintName != constraint {
		t.Fatalf("constraint = %q, want %q", pgErr.ConstraintName, constraint)
	}
}

func TestAuctionConstraints(t *testing.T) {
	tests := []struct {
		name       string
		sql        string
		code       string
		constraint string
	}{
		{"zero starting price", `INSERT INTO auctions (item_id, start_at, end_at, starting_price, min_increment)
			VALUES ($1, now(), now() + interval '1 hour', 0, 100)`, "23514", "auctions_starting_price_check"},
		{"zero increment", `INSERT INTO auctions (item_id, start_at, end_at, starting_price, min_increment)
			VALUES ($1, now(), now() + interval '1 hour', 1000, 0)`, "23514", "auctions_min_increment_check"},
		{"window ends before it starts", `INSERT INTO auctions (item_id, start_at, end_at, starting_price, min_increment)
			VALUES ($1, now(), now(), 1000, 100)`, "23514", "auctions_window"},
		{"unknown status", `INSERT INTO auctions (item_id, start_at, end_at, starting_price, min_increment, status)
			VALUES ($1, now(), now() + interval '1 hour', 1000, 100, 'paused')`, "23514", "auctions_status_check"},
		{"head half set", `INSERT INTO auctions (item_id, start_at, end_at, starting_price, min_increment, current_price)
			VALUES ($1, now(), now() + interval '1 hour', 1000, 100, 1000)`, "23514", "auctions_head_all_or_none"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			_, err := f.pool.Exec(t.Context(), tt.sql, f.item)
			wantPgError(t, err, tt.code, tt.constraint)
		})
	}
}

func TestAuctionHeadMustBeOwnBid(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	other := f.newAuction(t, "clock_timestamp() - interval '1 minute'", "clock_timestamp() + interval '1 hour'")
	bid, err := f.placeBid(ctx, other, f.users[0], 1000, nil, "k")
	if err != nil {
		t.Fatalf("place bid on other auction: %v", err)
	}

	// The update guard rejects it first: that bid does not extend this
	// auction's (empty) head.
	_, err = f.pool.Exec(ctx, `UPDATE auctions SET current_price = 1000, current_leader_id = $2, current_bid_id = $3
		WHERE id = $1`, f.auction, f.users[0], bid)
	wantPgError(t, err, "AE011", "")
}

// The head's leader and price must be exactly those of its bid. The bid here
// legitimately extends the head, so only the composite foreign key can
// catch the mismatch.
func TestAuctionHeadLeaderAndPriceMatchBid(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	err := pgx.BeginFunc(ctx, f.pool, func(tx pgx.Tx) error {
		var id int64
		if err := tx.QueryRow(ctx, `INSERT INTO bids (auction_id, user_id, amount, idempotency_key)
			VALUES ($1, $2, 1000, 'k') RETURNING id`, f.auction, f.users[0]).Scan(&id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO outbox (auction_id, event_type, payload, bid_id) VALUES ($1, 'bid_placed', '{}', $2)`, f.auction, id); err != nil {
			return err
		}
		// Wrong leader and inflated price for bid id.
		_, err := tx.Exec(ctx, `UPDATE auctions SET current_bid_id = $2, current_leader_id = $3, current_price = 999999 WHERE id = $1`,
			f.auction, id, f.users[1])
		return err
	})
	wantPgError(t, err, "23503", "auctions_head_is_own_bid")
}

func TestAuctionUpdateGuard(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	first, err := f.placeBid(ctx, f.auction, f.users[0], 1000, nil, "a")
	if err != nil {
		t.Fatalf("first bid: %v", err)
	}
	if _, err := f.placeBid(ctx, f.auction, f.users[1], 1100, &first, "b"); err != nil {
		t.Fatalf("second bid: %v", err)
	}

	// The head cannot move backwards to an earlier bid.
	_, err = f.pool.Exec(ctx, `UPDATE auctions SET current_bid_id = $2, current_leader_id = $3, current_price = 1000 WHERE id = $1`,
		f.auction, first, f.users[0])
	wantPgError(t, err, "AE011", "")

	// The head cannot be cleared.
	_, err = f.pool.Exec(ctx, `UPDATE auctions SET current_bid_id = NULL, current_leader_id = NULL, current_price = NULL WHERE id = $1`, f.auction)
	wantPgError(t, err, "AE011", "")

	// open -> closed is allowed (once ended, with its event: see
	// TestAuctionClose); closed -> open never is.
	ended := f.newAuction(t, "clock_timestamp() - interval '2 hours'", "clock_timestamp() - interval '1 hour'")
	if err := f.closeAuction(ctx, ended); err != nil {
		t.Fatalf("close auction: %v", err)
	}
	_, err = f.pool.Exec(ctx, `UPDATE auctions SET status = 'open' WHERE id = $1`, ended)
	wantPgError(t, err, "AE010", "")
}

func TestAuctionClose(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	ended := func() int64 {
		return f.newAuction(t, "clock_timestamp() - interval '2 hours'", "clock_timestamp() - interval '1 hour'")
	}

	// Not before end_at (f.auction ends in an hour).
	err := f.closeAuction(ctx, f.auction)
	wantPgError(t, err, "AE012", "")

	// Not without the event: the status change alone fails at COMMIT.
	a := ended()
	_, err = f.pool.Exec(ctx, `UPDATE auctions SET status = 'closed' WHERE id = $1`, a)
	wantPgError(t, err, "AE013", "")

	// No close event for an auction that is still open.
	_, err = f.pool.Exec(ctx, `INSERT INTO outbox (auction_id, event_type, payload) VALUES ($1, 'auction_closed', '{}')`, a)
	wantPgError(t, err, "AE014", "")

	// A correct close works, and a second close event is refused.
	if err := f.closeAuction(ctx, a); err != nil {
		t.Fatalf("close ended auction: %v", err)
	}
	_, err = f.pool.Exec(ctx, `INSERT INTO outbox (auction_id, event_type, payload) VALUES ($1, 'auction_closed', '{}')`, a)
	wantPgError(t, err, "23505", "outbox_one_close_per_auction")

	// Close events carry no bid.
	b := ended()
	err = pgx.BeginFunc(ctx, f.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE auctions SET status = 'closed' WHERE id = $1`, b); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO outbox (auction_id, event_type, payload, bid_id) VALUES ($1, 'auction_closed', '{}', $2)`,
			b, 1)
		return err
	})
	wantPgError(t, err, "23514", "outbox_bid_event_has_bid")

	// Unknown event types are still refused.
	_, err = f.pool.Exec(ctx, `INSERT INTO outbox (auction_id, event_type, payload) VALUES ($1, 'auction_reopened', '{}')`, a)
	wantPgError(t, err, "23514", "outbox_event_type_check")
}

func TestBidsAreAppendOnly(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	bid, err := f.placeBid(ctx, f.auction, f.users[0], 1000, nil, "a")
	if err != nil {
		t.Fatalf("bid: %v", err)
	}
	for name, sql := range map[string]string{
		"update":   `UPDATE bids SET amount = 1 WHERE id = $1`,
		"delete":   `DELETE FROM bids WHERE id = $1`,
		"truncate": `TRUNCATE bids CASCADE`,
	} {
		t.Run(name, func(t *testing.T) {
			args := []any{bid}
			if name == "truncate" {
				args = nil
			}
			_, err := f.pool.Exec(ctx, sql, args...)
			wantPgError(t, err, "AE008", "")
		})
	}
}

func TestOutboxIsAppendOnlyExceptPublishing(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	bid, err := f.placeBid(ctx, f.auction, f.users[0], 1000, nil, "a")
	if err != nil {
		t.Fatalf("bid: %v", err)
	}

	for name, sql := range map[string]string{
		"delete":          `DELETE FROM outbox WHERE bid_id = $1`,
		"change payload":  `UPDATE outbox SET payload = '{"forged": true}' WHERE bid_id = $1`,
		"publish+payload": `UPDATE outbox SET published_at = now(), payload = '{}'::jsonb || '{"x":1}' WHERE bid_id = $1`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := f.pool.Exec(ctx, sql, bid)
			wantPgError(t, err, "AE009", "")
		})
	}
	t.Run("truncate", func(t *testing.T) {
		_, err := f.pool.Exec(ctx, `TRUNCATE outbox`)
		wantPgError(t, err, "AE009", "")
	})

	// Marking published is allowed exactly once.
	if _, err := f.pool.Exec(ctx, `UPDATE outbox SET published_at = now() WHERE bid_id = $1`, bid); err != nil {
		t.Fatalf("mark published: %v", err)
	}
	_, err = f.pool.Exec(ctx, `UPDATE outbox SET published_at = now() WHERE bid_id = $1`, bid)
	wantPgError(t, err, "AE009", "")
}

// created_at is set by the guard to the time it judged the bid; a value
// supplied by the inserter is overwritten.
func TestBidCreatedAtCannotBeSupplied(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	var created time.Time
	err := pgx.BeginFunc(ctx, f.pool, func(tx pgx.Tx) error {
		var id int64
		if err := tx.QueryRow(ctx, `INSERT INTO bids (auction_id, user_id, amount, idempotency_key, created_at)
			VALUES ($1, $2, 1000, 'k', '2000-01-01') RETURNING id, created_at`, f.auction, f.users[0]).Scan(&id, &created); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO outbox (auction_id, event_type, payload, bid_id) VALUES ($1, 'bid_placed', '{}', $2)`, f.auction, id)
		return err
	})
	if err != nil {
		t.Fatalf("bid: %v", err)
	}
	if time.Since(created) > time.Minute {
		t.Errorf("created_at = %s; the supplied value was kept instead of the guard's clock", created)
	}
}

func TestAuctionPriceFloor(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	bid, err := f.placeBid(ctx, f.auction, f.users[0], 1000, nil, "k")
	if err != nil {
		t.Fatalf("place bid: %v", err)
	}
	_, err = f.pool.Exec(ctx, `UPDATE auctions SET current_price = 999 WHERE id = $1 AND current_bid_id = $2`, f.auction, bid)
	wantPgError(t, err, "23514", "auctions_price_floor")
}

func TestBidGuardTrigger(t *testing.T) {
	// Each case starts from an auction whose head is user 0's bid of 1000,
	// unless noHead is set.
	tests := []struct {
		name   string
		noHead bool
		user   int // index into fixture users
		amount int64
		// prevIsHead: extend the current head (correct); otherwise prev is nil.
		prevIsHead bool
		wantCode   string // "" means the bid must be accepted
	}{
		{name: "first bid below starting price", noHead: true, user: 0, amount: 999, wantCode: "AE006"},
		{name: "first bid at starting price", noHead: true, user: 0, amount: 1000},
		{name: "raise below increment", user: 1, amount: 1099, prevIsHead: true, wantCode: "AE006"},
		{name: "raise by exactly the increment", user: 1, amount: 1100, prevIsHead: true},
		{name: "leader outbids self", user: 0, amount: 5000, prevIsHead: true, wantCode: "AE005"},
		{name: "stale head (prev is not the current head)", user: 1, amount: 5000, prevIsHead: false, wantCode: "AE004"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			ctx := t.Context()
			var head *int64
			if !tt.noHead {
				id, err := f.placeBid(ctx, f.auction, f.users[0], 1000, nil, "setup")
				if err != nil {
					t.Fatalf("setup bid: %v", err)
				}
				head = &id
			}
			var prev *int64
			if tt.prevIsHead {
				prev = head
			}

			_, err := f.placeBid(ctx, f.auction, f.users[tt.user], tt.amount, prev, "k")

			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("bid rejected: %v", err)
				}
				return
			}
			wantPgError(t, err, tt.wantCode, "")
		})
	}
}

func TestBidGuardTimeWindowAndStatus(t *testing.T) {
	tests := []struct {
		name       string
		start, end string // SQL expressions
		closed     bool
		wantCode   string
	}{
		{"not started", "clock_timestamp() + interval '1 hour'", "clock_timestamp() + interval '2 hours'", false, "AE002"},
		{"ended", "clock_timestamp() - interval '2 hours'", "clock_timestamp() - interval '1 hour'", false, "AE003"},
		// A closed auction has always ended (AE012), so this also shows the
		// status check runs before the time check.
		{"closed status", "clock_timestamp() - interval '2 hours'", "clock_timestamp() - interval '1 hour'", true, "AE001"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			ctx := t.Context()
			a := f.newAuction(t, tt.start, tt.end)
			if tt.closed {
				if err := f.closeAuction(ctx, a); err != nil {
					t.Fatalf("close auction: %v", err)
				}
			}
			_, err := f.placeBid(ctx, a, f.users[0], 1000, nil, "k")
			wantPgError(t, err, tt.wantCode, "")
		})
	}
}

// With the guard trigger disabled, the chain's unique constraint alone must
// still stop two bids from extending the same predecessor: the layer that
// makes a lost update impossible even if every lock were missing.
func TestChainCannotFork(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	if _, err := f.pool.Exec(ctx, `ALTER TABLE bids DISABLE TRIGGER bids_guard`); err != nil {
		t.Fatalf("disable guard: %v", err)
	}
	first, err := f.placeBid(ctx, f.auction, f.users[0], 1000, nil, "a")
	if err != nil {
		t.Fatalf("first bid: %v", err)
	}
	if _, err := f.placeBid(ctx, f.auction, f.users[1], 1100, &first, "b"); err != nil {
		t.Fatalf("second bid: %v", err)
	}

	_, err = f.placeBid(ctx, f.auction, f.users[2], 1200, &first, "c")
	wantPgError(t, err, "23505", "bids_chain_no_fork")

	_, err = f.placeBid(ctx, f.auction, f.users[2], 1200, nil, "d")
	wantPgError(t, err, "23505", "bids_chain_no_fork")
}

func TestBidPrevMustBeSameAuction(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	if _, err := f.pool.Exec(ctx, `ALTER TABLE bids DISABLE TRIGGER bids_guard`); err != nil {
		t.Fatalf("disable guard: %v", err)
	}
	other := f.newAuction(t, "clock_timestamp() - interval '1 minute'", "clock_timestamp() + interval '1 hour'")
	foreign, err := f.placeBid(ctx, other, f.users[0], 1000, nil, "a")
	if err != nil {
		t.Fatalf("bid on other auction: %v", err)
	}
	_, err = f.placeBid(ctx, f.auction, f.users[1], 1100, &foreign, "b")
	wantPgError(t, err, "23503", "bids_prev_same_auction")
}

func TestIdempotencyKeyScopedPerUser(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	first, err := f.placeBid(ctx, f.auction, f.users[0], 1000, nil, "same-key")
	if err != nil {
		t.Fatalf("first bid: %v", err)
	}
	// A different user may reuse the key.
	second, err := f.placeBid(ctx, f.auction, f.users[1], 1100, &first, "same-key")
	if err != nil {
		t.Fatalf("other user reusing the key was rejected: %v", err)
	}
	// The same user may not.
	_, err = f.placeBid(ctx, f.auction, f.users[0], 1200, &second, "same-key")
	wantPgError(t, err, "23505", "bids_idempotency")
}

// Invariant 6, "no bid without its outbox event": a transaction that inserts
// a bid but no event must fail at COMMIT and leave nothing behind.
func TestBidWithoutOutboxFailsAtCommit(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()

	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO bids (auction_id, user_id, amount, idempotency_key) VALUES ($1, $2, 1000, 'k')`,
		f.auction, f.users[0]); err != nil {
		t.Fatalf("insert bid (the check is deferred, so this must succeed): %v", err)
	}
	err = tx.Commit(ctx)
	wantPgError(t, err, "AE007", "")

	var n int
	mustScan(t, f.pool.QueryRow(ctx, `SELECT count(*) FROM bids`), &n)
	if n != 0 {
		t.Errorf("%d bids persisted after the failed commit, want 0", n)
	}
}

// Invariant 6, "no outbox event without its bid".
func TestOutboxConstraints(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	other := f.newAuction(t, "clock_timestamp() - interval '1 minute'", "clock_timestamp() + interval '1 hour'")
	bid, err := f.placeBid(ctx, f.auction, f.users[0], 1000, nil, "k")
	if err != nil {
		t.Fatalf("place bid: %v", err)
	}

	_, err = f.pool.Exec(ctx, `INSERT INTO outbox (auction_id, event_type, payload, bid_id) VALUES ($1, 'bid_placed', '{}', 999999)`, f.auction)
	wantPgError(t, err, "23503", "outbox_bid_of_auction")

	// An event filed under the wrong auction. The bid must be fresh (no
	// event yet); reusing `bid` would be rejected by UNIQUE (bid_id) first,
	// which Postgres checks before the foreign key.
	err = pgx.BeginFunc(ctx, f.pool, func(tx pgx.Tx) error {
		var fresh int64
		if err := tx.QueryRow(ctx, `INSERT INTO bids (auction_id, user_id, amount, idempotency_key)
			VALUES ($1, $2, 1000, 'fresh') RETURNING id`, other, f.users[1]).Scan(&fresh); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO outbox (auction_id, event_type, payload, bid_id) VALUES ($1, 'bid_placed', '{}', $2)`, f.auction, fresh)
		return err
	})
	wantPgError(t, err, "23503", "outbox_bid_of_auction")

	_, err = f.pool.Exec(ctx, `INSERT INTO outbox (auction_id, event_type, payload) VALUES ($1, 'bid_placed', '{}')`, f.auction)
	wantPgError(t, err, "23514", "outbox_bid_event_has_bid")

	_, err = f.pool.Exec(ctx, `INSERT INTO outbox (auction_id, event_type, payload, bid_id) VALUES ($1, 'bid_placed', '{}', $2)`, f.auction, bid)
	wantPgError(t, err, "23505", "outbox_bid_id_key")
}

// The relay's poll must be able to use the partial index. Tables in tests
// are tiny, so the planner would pick a sequential scan anyway; disabling
// that shows whether the index is usable at all, i.e. whether its
// predicate matches the query's WHERE clause.
func TestOutboxRelayQueryUsesPartialIndex(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	err := pgx.BeginFunc(ctx, f.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
			return err
		}
		// EXPLAIN returns one row per plan line.
		rows, err := tx.Query(ctx, `EXPLAIN (FORMAT TEXT)
			SELECT id FROM outbox WHERE published_at IS NULL ORDER BY id LIMIT 100`)
		if err != nil {
			return err
		}
		lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		plan := strings.Join(lines, "\n")
		if !strings.Contains(plan, "outbox_unpublished") {
			t.Errorf("relay query plan does not use outbox_unpublished:\n%s", plan)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
}

// created_at must be the moment the bid was inserted, not the start of its
// transaction, so bids that waited on a lock are ordered correctly.
func TestBidCreatedAtIsInsertTime(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	var txStart, created time.Time
	err := pgx.BeginFunc(ctx, f.pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&txStart); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT pg_sleep(0.2)`); err != nil {
			return err
		}
		var id int64
		if err := tx.QueryRow(ctx, `INSERT INTO bids (auction_id, user_id, amount, idempotency_key)
			VALUES ($1, $2, 1000, 'k') RETURNING id, created_at`, f.auction, f.users[0]).Scan(&id, &created); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO outbox (auction_id, event_type, payload, bid_id) VALUES ($1, 'bid_placed', '{}', $2)`, f.auction, id)
		return err
	})
	if err != nil {
		t.Fatalf("bid transaction: %v", err)
	}
	if gap := created.Sub(txStart); gap < 200*time.Millisecond {
		t.Errorf("created_at is %s after transaction start, want >= 200ms (was now() used?)", gap)
	}
}

// endedWithBids returns an auction with two accepted bids (users 0 then 1)
// that has ended and been closed, and its bids' ids.
func (f *fixture) endedWithBids(t *testing.T) (auction, first, second int64) {
	t.Helper()
	ctx := t.Context()
	auction = f.newAuction(t, "clock_timestamp() - interval '1 minute'", "clock_timestamp() + interval '1 hour'")
	first, err := f.placeBid(ctx, auction, f.users[0], 1000, nil, fmt.Sprintf("a%d-1", auction))
	if err != nil {
		t.Fatalf("first bid: %v", err)
	}
	second, err = f.placeBid(ctx, auction, f.users[1], 1100, &first, fmt.Sprintf("a%d-2", auction))
	if err != nil {
		t.Fatalf("second bid: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE auctions SET end_at = clock_timestamp() WHERE id = $1`, auction); err != nil {
		t.Fatalf("end auction: %v", err)
	}
	if err := f.closeAuction(ctx, auction); err != nil {
		t.Fatalf("close auction: %v", err)
	}
	return auction, first, second
}

func (f *fixture) insertInvoice(ctx context.Context, auction, bid, winner, amount int64) (int64, error) {
	var id int64
	err := f.pool.QueryRow(ctx, `
		INSERT INTO invoices (auction_id, bid_id, winner_id, amount) VALUES ($1, $2, $3, $4) RETURNING id`,
		auction, bid, winner, amount).Scan(&id)
	return id, err
}

func TestInvoiceMustMatchClosedResult(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()

	// The fixture's own auction is still open.
	open, err := f.placeBid(ctx, f.auction, f.users[0], 1000, nil, "open")
	if err != nil {
		t.Fatalf("bid: %v", err)
	}
	_, err = f.insertInvoice(ctx, f.auction, open, f.users[0], 1000)
	wantPgError(t, err, "AE015", "")

	a, first, second := f.endedWithBids(t)
	for _, tc := range []struct {
		name                string
		bid, winner, amount int64
	}{
		{"an earlier bid", first, f.users[0], 1000},
		{"the wrong winner", second, f.users[0], 1100},
		{"the wrong amount", second, f.users[1], 1},
	} {
		_, err := f.insertInvoice(ctx, a, tc.bid, tc.winner, tc.amount)
		if err == nil {
			t.Fatalf("invoice with %s was accepted", tc.name)
		}
		wantPgError(t, err, "AE015", "")
	}

	if _, err := f.insertInvoice(ctx, a, second, f.users[1], 1100); err != nil {
		t.Fatalf("matching invoice: %v", err)
	}
	_, err = f.insertInvoice(ctx, a, second, f.users[1], 1100)
	wantPgError(t, err, "23505", "invoices_auction_id_key")
}

func TestInvoiceLifecycle(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	a, _, second := f.endedWithBids(t)
	inv, err := f.insertInvoice(ctx, a, second, f.users[1], 1100)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	exec := func(sql string) error {
		_, err := f.pool.Exec(ctx, sql, inv)
		return err
	}

	wantPgError(t, exec(`UPDATE invoices SET amount = 1 WHERE id = $1`), "AE016", "")
	wantPgError(t, exec(`UPDATE invoices SET winner_id = winner_id WHERE id = $1`), "AE016", "") // no status move
	wantPgError(t, exec(`UPDATE invoices SET status = 'paid', settled_at = clock_timestamp() WHERE id = $1`),
		"23514", "invoices_paid_has_payment")
	wantPgError(t, exec(`UPDATE invoices SET status = 'paid', payment_id = 'ch_1' WHERE id = $1`),
		"23514", "invoices_settled_when_not_pending")

	// pending -> paid.
	if err := exec(`UPDATE invoices SET status = 'paid', payment_id = 'ch_1', settled_at = clock_timestamp() WHERE id = $1`); err != nil {
		t.Fatalf("pay: %v", err)
	}

	// paid is final, and invoices are never deleted.
	wantPgError(t, exec(`UPDATE invoices SET status = 'pending', payment_id = NULL, settled_at = NULL WHERE id = $1`), "AE016", "")
	wantPgError(t, exec(`UPDATE invoices SET status = 'failed', payment_id = NULL, failure = 'x' WHERE id = $1`), "AE016", "")
	wantPgError(t, exec(`DELETE FROM invoices WHERE id = $1`), "AE016", "")
	_, err = f.pool.Exec(ctx, `TRUNCATE invoices`)
	wantPgError(t, err, "AE016", "")

	// pending -> failed, and failed is final too.
	b, _, winning := f.endedWithBids(t)
	declined, err := f.insertInvoice(ctx, b, winning, f.users[1], 1100)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE invoices SET status = 'failed', failure = 'declined', settled_at = clock_timestamp() WHERE id = $1`, declined); err != nil {
		t.Fatalf("decline: %v", err)
	}
	for _, sql := range []string{
		`UPDATE invoices SET status = 'pending', failure = NULL, settled_at = NULL WHERE id = $1`,
		`UPDATE invoices SET status = 'paid', failure = NULL, payment_id = 'ch_2' WHERE id = $1`,
	} {
		_, err := f.pool.Exec(ctx, sql, declined)
		wantPgError(t, err, "AE016", "")
	}
}
