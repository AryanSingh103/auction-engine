package auction_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AryanSingh103/auction-engine/internal/auction"
)

// closeEvents returns the auction_closed payloads written for an auction.
func closeEvents(t *testing.T, pool *pgxpool.Pool, auctionID int64) []auction.ClosedEvent {
	t.Helper()
	rows, err := pool.Query(t.Context(), `SELECT payload FROM outbox WHERE auction_id = $1 AND event_type = 'auction_closed'`, auctionID)
	if err != nil {
		t.Fatalf("read close events: %v", err)
	}
	defer rows.Close()
	var out []auction.ClosedEvent
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("scan: %v", err)
		}
		var ev auction.ClosedEvent
		if err := json.Unmarshal(raw, &ev); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

func TestCloseAuctionBeforeEndIsRefused(t *testing.T) {
	pool := server.NewDB(t, 4)
	id := seed(t, pool, 1, "clock_timestamp() + interval '1 hour'")
	svc := auction.NewService(pool)
	if _, err := svc.CloseAuction(t.Context(), id); !errors.Is(err, auction.ErrAuctionNotEnded) {
		t.Fatalf("close before end: %v, want ErrAuctionNotEnded", err)
	}
	if evs := closeEvents(t, pool, id); len(evs) != 0 {
		t.Fatalf("%d close events after a refused close", len(evs))
	}
}

func TestCloseAuctionUnknown(t *testing.T) {
	svc := auction.NewService(server.NewDB(t, 4))
	if _, err := svc.CloseAuction(t.Context(), 424242); !errors.Is(err, auction.ErrAuctionNotFound) {
		t.Fatalf("close unknown: %v, want ErrAuctionNotFound", err)
	}
}

func TestCloseAuctionWithoutBids(t *testing.T) {
	pool := server.NewDB(t, 4)
	id := seed(t, pool, 1, "clock_timestamp() + interval '1 hour'")
	if _, err := pool.Exec(t.Context(), `UPDATE auctions SET start_at = clock_timestamp() - interval '2 hours', end_at = clock_timestamp() - interval '1 hour' WHERE id = $1`, id); err != nil {
		t.Fatalf("move window: %v", err)
	}
	svc := auction.NewService(pool)
	closed, err := svc.CloseAuction(t.Context(), id)
	if err != nil || !closed {
		t.Fatalf("close = %v, %v; want true, nil", closed, err)
	}
	evs := closeEvents(t, pool, id)
	if len(evs) != 1 || evs[0].WinnerID != nil || evs[0].BidID != nil || evs[0].Amount != nil {
		t.Fatalf("close events = %+v, want one with no winner", evs)
	}
	assertInvariants(t, pool)
}

// Bidders keep bidding right up to and past the end while the auction is
// closed as soon as it can be. The close event must name exactly the final
// head, no bid may be accepted after the close, and a second close is a
// no-op.
func TestCloseAuctionRacingBids(t *testing.T) {
	const bidders = 8
	pool := server.NewDB(t, 20)
	id := seed(t, pool, bidders, "clock_timestamp() + interval '400 milliseconds'")
	svc := auction.NewService(pool)

	var closedFlag atomic.Bool
	var acceptedAfterClose atomic.Int64
	var wg sync.WaitGroup
	for u := range bidders {
		wg.Go(func() {
			user := int64(u + 1)
			for i := 0; ; i++ {
				wasClosed := closedFlag.Load()
				a, err := svc.GetAuction(t.Context(), id)
				if err != nil {
					t.Errorf("get: %v", err)
					return
				}
				_, _, err = svc.PlaceBid(t.Context(), auction.PlaceBidRequest{
					AuctionID: id, UserID: user, Amount: a.MinimumBid(),
					IdempotencyKey: fmt.Sprintf("u%d-%d", user, i),
				})
				if err == nil && wasClosed {
					acceptedAfterClose.Add(1)
				}
				if wasClosed {
					return // one attempt after the close is enough
				}
			}
		})
	}

	// Close as soon as the auction has ended: poll, since the end is a
	// database timestamp.
	deadline := time.Now().Add(10 * time.Second)
	for {
		closed, err := svc.CloseAuction(t.Context(), id)
		if err == nil && closed {
			break
		}
		if err != nil && !errors.Is(err, auction.ErrAuctionNotEnded) {
			t.Fatalf("close: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("auction did not become closable within 10s")
		}
		time.Sleep(5 * time.Millisecond)
	}
	closedFlag.Store(true)
	wg.Wait()

	if n := acceptedAfterClose.Load(); n != 0 {
		t.Errorf("%d bids accepted after the close committed", n)
	}
	a, err := svc.GetAuction(t.Context(), id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if a.Status != auction.StatusClosed || a.Head == nil {
		t.Fatalf("auction after close = %+v, want closed with a head", a)
	}
	evs := closeEvents(t, pool, id)
	if len(evs) != 1 {
		t.Fatalf("%d close events, want 1", len(evs))
	}
	ev := evs[0]
	if ev.BidID == nil || *ev.BidID != a.Head.BidID || *ev.WinnerID != a.Head.UserID || *ev.Amount != a.Head.Price {
		t.Errorf("close event %+v does not name the final head %+v", ev, *a.Head)
	}
	if closed, err := svc.CloseAuction(t.Context(), id); err != nil || closed {
		t.Errorf("second close = %v, %v; want false, nil", closed, err)
	}
	if n := len(closeEvents(t, pool, id)); n != 1 {
		t.Errorf("%d close events after a second close, want 1", n)
	}
	t.Logf("final head: bid %d by user %d at %d", a.Head.BidID, a.Head.UserID, a.Head.Price)
	assertInvariants(t, pool)
}

// The case the close's row lock exists for, forced deterministically: a
// bid passes its time check before the end, then is still in flight when
// the close starts. The test pauses it by locking the bidder's users row:
// the bid's INSERT runs the guard trigger (auction row lock and time check)
// before its foreign key check, which then waits for that row. Without the
// row lock, the close would read the head before this bid and name the
// wrong winner. The race test above does not catch that reliably (0 of 20
// runs without the lock); this one does, every time.
func TestCloseWaitsForInFlightBid(t *testing.T) { forEachLocking(t, testCloseWaitsForInFlightBid) }

func testCloseWaitsForInFlightBid(t *testing.T, locking auction.Locking) {
	ctx := t.Context()
	pool := server.NewDB(t, 6)
	id := seed(t, pool, 1, "clock_timestamp() + interval '1 second'")
	svc := auction.NewService(pool, auction.WithLocking(locking))

	userLock, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = userLock.Rollback(ctx) }()
	if _, err := userLock.Exec(ctx, `SELECT 1 FROM users WHERE id = 1 FOR UPDATE`); err != nil {
		t.Fatalf("lock user: %v", err)
	}

	var bidErr error
	var bid auction.Bid
	var wg sync.WaitGroup
	wg.Go(func() {
		bid, _, bidErr = svc.PlaceBid(ctx, auction.PlaceBidRequest{
			AuctionID: id, UserID: 1, Amount: startingPrice, IdempotencyKey: "in-flight",
		})
	})
	waitForLockWaiters(t, pool, 1) // the bid, on the users row

	// Let the auction end while the bid is still in flight.
	for {
		var ended bool
		if err := pool.QueryRow(ctx, `SELECT clock_timestamp() >= end_at FROM auctions WHERE id = $1`, id).Scan(&ended); err != nil {
			t.Fatalf("read end: %v", err)
		}
		if ended {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	var closed bool
	var closeErr error
	wg.Go(func() { closed, closeErr = svc.CloseAuction(ctx, id) })
	waitForLockWaiters(t, pool, 2) // plus the close, on the auction row

	if err := userLock.Rollback(ctx); err != nil {
		t.Fatalf("release user: %v", err)
	}
	wg.Wait()

	if bidErr != nil {
		t.Fatalf("in-flight bid: %v, want accepted (it was decided before the end)", bidErr)
	}
	if closeErr != nil || !closed {
		t.Fatalf("close = %v, %v; want true, nil", closed, closeErr)
	}
	evs := closeEvents(t, pool, id)
	if len(evs) != 1 || evs[0].BidID == nil || *evs[0].BidID != bid.ID {
		t.Fatalf("close events = %+v, want one naming the in-flight bid %d", evs, bid.ID)
	}
	assertInvariants(t, pool)
}

// waitForLockWaiters waits until n sessions in this database are blocked
// on a lock.
func waitForLockWaiters(t *testing.T, pool *pgxpool.Pool, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := pool.QueryRow(t.Context(), `
			SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			t.Fatalf("count lock waiters: %v", err)
		}
		if waiting == n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d session(s) waiting on locks after 10s, want %d", waiting, n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
