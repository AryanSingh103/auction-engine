package auction_test

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AryanSingh103/auction-engine/internal/auction"
)

// snipeAuction creates an auction ending at endExpr with the given
// anti-snipe terms (SQL interval literals), and users 1..users if absent.
func snipeAuction(t *testing.T, pool *pgxpool.Pool, users int, endExpr, window, extend string) int64 {
	t.Helper()
	ctx := t.Context()
	if _, err := pool.Exec(ctx, `
		INSERT INTO users (id, name) OVERRIDING SYSTEM VALUE
		SELECT g, 'user ' || g FROM generate_series(1, $1) g ON CONFLICT DO NOTHING`, users); err != nil {
		t.Fatalf("seed users: %v", err)
	}
	var id int64
	err := pool.QueryRow(ctx, fmt.Sprintf(`
		WITH item AS (INSERT INTO items (title) VALUES ('unit') RETURNING id)
		INSERT INTO auctions (item_id, start_at, end_at, starting_price, min_increment, extend_window, extend_by)
		SELECT id, clock_timestamp() - interval '1 minute', %s, $1, $2, $3::interval, $4::interval FROM item
		RETURNING id`, endExpr), startingPrice, increment, window, extend).Scan(&id)
	if err != nil {
		t.Fatalf("seed auction: %v", err)
	}
	return id
}

func TestAntiSnipe(t *testing.T) {
	forEachLocking(t, func(t *testing.T, l auction.Locking) {
		pool := server.NewDB(t, 4)
		svc := auction.NewService(pool, auction.WithLocking(l))
		ctx := t.Context()
		bid := func(id int64) (auction.Bid, auction.Auction) {
			t.Helper()
			before, err := svc.ReadAuction(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			b, _, err := svc.PlaceBid(ctx, auction.PlaceBidRequest{AuctionID: id, UserID: 1, Amount: startingPrice, IdempotencyKey: fmt.Sprint(id)})
			if err != nil {
				t.Fatalf("bid: %v", err)
			}
			return b, before
		}
		read := func(id int64) auction.Auction {
			a, err := svc.ReadAuction(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			return a
		}

		// Outside the window: the end stays.
		far := snipeAuction(t, pool, 1, "clock_timestamp() + interval '1 hour'", "10 minutes", "30 minutes")
		_, before := bid(far)
		if after := read(far); !after.EndAt.Equal(before.EndAt) || after.Version != before.Version+1 {
			t.Errorf("bid outside the window: end %s -> %s, version %d -> %d", before.EndAt, after.EndAt, before.Version, after.Version)
		}

		// Inside it: the end becomes bid time + extend_by.
		near := snipeAuction(t, pool, 1, "clock_timestamp() + interval '5 minutes'", "10 minutes", "30 minutes")
		b, _ := bid(near)
		if after := read(near); !after.EndAt.Equal(b.CreatedAt.Add(30 * time.Minute)) {
			t.Errorf("bid inside the window: end %s, want bid time %s + 30m", after.EndAt, b.CreatedAt)
		}

		// Inside it, but bid time + extend_by is before the end: the end stays.
		short := snipeAuction(t, pool, 1, "clock_timestamp() + interval '5 minutes'", "10 minutes", "1 minute")
		_, before = bid(short)
		if after := read(short); !after.EndAt.Equal(before.EndAt) {
			t.Errorf("short extension moved the end: %s -> %s", before.EndAt, after.EndAt)
		}
	})
}

// The close-vs-bid race, forced deterministically: a bid is accepted
// (inside the anti-snipe window) just before the end, and its transaction
// is still open when the end passes and a close arrives. The close must
// wait for the row lock, then see the extended end and refuse; it must
// never close on the stale end it would have seen without the lock.
func TestCloseWaitsForBidInFlightAndSeesItsExtension(t *testing.T) {
	pool := server.NewDB(t, 6)
	svc := auction.NewService(pool)
	ctx := t.Context()
	id := snipeAuction(t, pool, 1, "clock_timestamp() + interval '500 milliseconds'", "1 hour", "2 seconds")
	original, err := svc.ReadAuction(ctx, id)
	if err != nil {
		t.Fatal(err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	bid, err := auction.PlaceBidInTx(ctx, tx, auction.PlaceBidRequest{AuctionID: id, UserID: 1, Amount: startingPrice, IdempotencyKey: "k"})
	if err != nil {
		t.Fatalf("bid: %v", err)
	}
	if !bid.CreatedAt.Before(original.EndAt) {
		t.Fatalf("bid at %s, not before the end %s", bid.CreatedAt, original.EndAt)
	}

	// Let the original end pass (by the database clock) with the bid
	// uncommitted, then start the close.
	waitFor(t, "the original end to pass", func() bool {
		var past bool
		if err := pool.QueryRow(ctx, `SELECT clock_timestamp() > $1`, original.EndAt).Scan(&past); err != nil {
			t.Fatal(err)
		}
		return past
	})
	closeErr := make(chan error, 1)
	go func() {
		_, err := svc.CloseAuction(ctx, id)
		closeErr <- err
	}()
	waitForLockWaiters(t, pool, 1)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit bid: %v", err)
	}

	if err := <-closeErr; !errors.Is(err, auction.ErrAuctionNotEnded) {
		t.Fatalf("close after the original end = %v, want ErrAuctionNotEnded (the bid extended it)", err)
	}
	a, err := svc.ReadAuction(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != auction.StatusOpen || !a.EndAt.Equal(bid.CreatedAt.Add(2*time.Second)) {
		t.Fatalf("after the refused close: status %s, end %s; want open, %s", a.Status, a.EndAt, bid.CreatedAt.Add(2*time.Second))
	}

	// Once the extended end passes, the close goes through with that bid.
	waitFor(t, "the extended auction to close", func() bool {
		closed, err := svc.CloseAuction(ctx, id)
		if err != nil && !errors.Is(err, auction.ErrAuctionNotEnded) {
			t.Fatalf("close: %v", err)
		}
		return closed
	})
	if evs := closeEvents(t, pool, id); len(evs) != 1 || evs[0].BidID == nil || *evs[0].BidID != bid.ID {
		t.Fatalf("close events = %+v, want one won by bid %d", evs, bid.ID)
	}
	assertInvariants(t, pool)
}

// Many auctions end while bidders are still bidding (with anti-snipe) and
// two "closers" call CloseAuction in a loop, as a split brain would. Every
// auction must close exactly once, after its final end and its last bid,
// won by the highest bid any client was told was accepted.
func TestCloseVersusBidStress(t *testing.T) {
	forEachLocking(t, func(t *testing.T, l auction.Locking) {
		const (
			auctions = 8
			bidders  = 3 // per auction
			users    = 20
		)
		pool := server.NewDB(t, 40)
		svc := auction.NewService(pool, auction.WithLocking(l))
		ctx := t.Context()

		ids := make([]int64, auctions)
		originalEnd := make(map[int64]time.Time)
		for i := range ids {
			ids[i] = snipeAuction(t, pool, users, "clock_timestamp() + interval '400 milliseconds'", "150 milliseconds", "150 milliseconds")
			a, err := svc.ReadAuction(ctx, ids[i])
			if err != nil {
				t.Fatal(err)
			}
			originalEnd[ids[i]] = a.EndAt
		}

		var (
			mu      sync.Mutex
			highest = map[int64]auction.Bid{} // highest accepted bid per auction, as clients saw it
			ended   atomic.Int64              // refused: past the end, not yet closed
			closed  atomic.Int64              // refused: already closed
			refused atomic.Int64              // closes refused because the end had moved
		)
		stopBidding := time.Now().Add(3 * time.Second)
		var wg sync.WaitGroup
		for _, id := range ids {
			for range bidders {
				wg.Go(func() {
					for n := 0; time.Now().Before(stopBidding); n++ {
						time.Sleep(time.Duration(rand.IntN(400)) * time.Millisecond)
						a, err := svc.ReadAuction(ctx, id)
						if err != nil {
							t.Error(err)
							return
						}
						user := int64(rand.IntN(users) + 1)
						b, _, err := svc.PlaceBid(ctx, auction.PlaceBidRequest{AuctionID: id, UserID: user,
							Amount: a.MinimumBid(), IdempotencyKey: fmt.Sprintf("%d-%d-%d", id, user, rand.Int64())})
						switch {
						case err == nil:
							mu.Lock()
							if b.Amount > highest[id].Amount {
								highest[id] = b
							}
							mu.Unlock()
						case errors.Is(err, auction.ErrAuctionEnded):
							ended.Add(1)
						case errors.Is(err, auction.ErrAuctionNotOpen):
							if errors.Is(err, auction.ErrRejectedByDatabaseGuard) {
								t.Errorf("guard caught what the Go rules missed: %v", err)
							}
							closed.Add(1)
						case errors.Is(err, auction.ErrBidTooLow), errors.Is(err, auction.ErrSelfOutbid):
							// lost a race to another bidder
							if errors.Is(err, auction.ErrRejectedByDatabaseGuard) {
								t.Errorf("guard caught what the Go rules missed: %v", err)
							}
						default:
							t.Errorf("bid: %v", err)
							return
						}
					}
				})
			}
		}
		for range 2 {
			wg.Go(func() {
				deadline := time.Now().Add(15 * time.Second)
				for open := len(ids); open > 0 && time.Now().Before(deadline); {
					open = 0
					for _, id := range ids {
						closed, err := svc.CloseAuction(ctx, id)
						switch {
						case errors.Is(err, auction.ErrAuctionNotEnded):
							open++
							refused.Add(1)
						case err != nil:
							t.Errorf("close %d: %v", id, err)
						case !closed:
							if a, err := svc.ReadAuction(ctx, id); err == nil && a.Status != auction.StatusClosed {
								t.Errorf("close %d reported no-op on an open auction", id)
							}
						}
					}
					time.Sleep(5 * time.Millisecond)
				}
			})
		}
		wg.Wait()

		extended := 0
		for _, id := range ids {
			a, err := svc.ReadAuction(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if a.Status != auction.StatusClosed {
				t.Fatalf("auction %d still open", id)
			}
			if a.EndAt.After(originalEnd[id]) {
				extended++
			}
			evs := closeEvents(t, pool, id)
			want := highest[id]
			if len(evs) != 1 {
				t.Fatalf("auction %d: %d close events", id, len(evs))
			}
			if want.ID == 0 {
				if evs[0].BidID != nil {
					t.Errorf("auction %d: no accepted bid, but won by %d", id, *evs[0].BidID)
				}
			} else if evs[0].BidID == nil || *evs[0].BidID != want.ID || *evs[0].Amount != want.Amount {
				t.Errorf("auction %d: close event %+v, want won by bid %d (%d)", id, evs[0], want.ID, want.Amount)
			}
		}
		assertInvariants(t, pool)
		t.Logf("%d/%d auctions extended; bids refused: %d ended, %d closed; %d closes refused as not ended",
			extended, auctions, ended.Load(), closed.Load(), refused.Load())
		// A run that never raced proves nothing: ends must have moved under
		// the closers, and bids must have arrived after the end.
		if extended == 0 || refused.Load() == 0 || ended.Load()+closed.Load() == 0 {
			t.Fatal("the race was not exercised")
		}
	})
}

// waitFor polls cond until it holds or 10s pass.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
