package settlement_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AryanSingh103/auction-engine/internal/auction"
	"github.com/AryanSingh103/auction-engine/internal/breaker"
	"github.com/AryanSingh103/auction-engine/internal/paysim"
	"github.com/AryanSingh103/auction-engine/internal/settlement"
	"github.com/AryanSingh103/auction-engine/internal/testdb"
	"github.com/AryanSingh103/auction-engine/internal/testkafka"
)

var (
	db    *testdb.Server
	kafka *testkafka.Server
)

func TestMain(m *testing.M) {
	ctx := context.Background()
	var err error
	if db, err = testdb.Start(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "settlement tests: %v\n", err)
		os.Exit(1)
	}
	if kafka, err = testkafka.Start(ctx); err != nil {
		_ = db.Terminate(ctx)
		fmt.Fprintf(os.Stderr, "settlement tests: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	_ = db.Terminate(ctx)
	_ = kafka.Terminate(ctx)
	os.Exit(code)
}

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

// closedAuction creates an auction won by user 2 at `amount` (user 1 bid
// first, one increment below) and closes it. amount 0 means no bids.
func closedAuction(t *testing.T, pool *pgxpool.Pool, amount int64) int64 {
	t.Helper()
	ctx := t.Context()
	if _, err := pool.Exec(ctx, `INSERT INTO users (name) SELECT 'u' || g FROM generate_series(1, 2) g ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("seed users: %v", err)
	}
	var id int64
	err := pool.QueryRow(ctx, `
		WITH item AS (INSERT INTO items (title) VALUES ('unit') RETURNING id)
		INSERT INTO auctions (item_id, start_at, end_at, starting_price, min_increment)
		SELECT id, clock_timestamp() - interval '1 minute', clock_timestamp() + interval '1 hour', 100, 1 FROM item
		RETURNING id`).Scan(&id)
	if err != nil {
		t.Fatalf("seed auction: %v", err)
	}
	svc := auction.NewService(pool)
	if amount > 0 {
		for i, a := range []int64{amount - 1, amount} {
			if _, _, err := svc.PlaceBid(ctx, auction.PlaceBidRequest{AuctionID: id, UserID: int64(i + 1), Amount: a,
				IdempotencyKey: fmt.Sprintf("a%d-%d", id, i)}); err != nil {
				t.Fatalf("bid %d: %v", a, err)
			}
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE auctions SET end_at = clock_timestamp() WHERE id = $1`, id); err != nil {
		t.Fatalf("end auction: %v", err)
	}
	if _, err := svc.CloseAuction(ctx, id); err != nil {
		t.Fatalf("close: %v", err)
	}
	return id
}

// provider runs the real simulator handler. Each request takes the next
// roll from rolls; once they run out, requests succeed.
func provider(t *testing.T, pool *pgxpool.Pool, hang time.Duration, rolls ...float64) string {
	t.Helper()
	store := paysim.NewStore(pool)
	if err := store.Migrate(t.Context()); err != nil {
		t.Fatalf("migrate paysim: %v", err)
	}
	var mu sync.Mutex
	roll := func() float64 {
		mu.Lock()
		defer mu.Unlock()
		if len(rolls) == 0 {
			return 0.99
		}
		r := rolls[0]
		rolls = rolls[1:]
		return r
	}
	faults := paysim.Faults{FailureRate: 0.1, HangRate: 0.1, Hang: hang, Roll: roll}
	srv := httptest.NewServer(paysim.NewHandler(store, faults, discard))
	t.Cleanup(srv.Close)
	return srv.URL
}

const (
	failBefore = 0.0  // 503, nothing charged
	lostAfter  = 0.07 // charged, then 500
	hangAfter  = 0.15 // charged, then hangs
)

func newSettler(pool *pgxpool.Pool, url string, maxAttempts int) *settlement.Settler {
	b := breaker.New(1000, time.Second, time.Now, nil) // effectively off; tested on its own
	return settlement.NewSettler(pool, settlement.NewPaymentClient(url, 200*time.Millisecond), b, maxAttempts, time.Millisecond, 5*time.Millisecond)
}

type ledger struct {
	invoices int
	status   string
	payment  *string
	charges  int
	chargeID *string
}

// books reads our invoice and the provider's charges for an auction.
func books(t *testing.T, pool *pgxpool.Pool, auctionID int64) ledger {
	t.Helper()
	var l ledger
	ctx := t.Context()
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM invoices WHERE auction_id = $1`, auctionID).Scan(&l.invoices); err != nil {
		t.Fatalf("count invoices: %v", err)
	}
	if l.invoices == 1 {
		if err := pool.QueryRow(ctx, `SELECT status, payment_id FROM invoices WHERE auction_id = $1`, auctionID).Scan(&l.status, &l.payment); err != nil {
			t.Fatalf("read invoice: %v", err)
		}
	}
	if err := pool.QueryRow(ctx, `
		SELECT count(*), min(c.id) FROM paysim.charges c
		JOIN invoices i ON c.idempotency_key = 'invoice-' || i.id
		WHERE i.auction_id = $1`, auctionID).Scan(&l.charges, &l.chargeID); err != nil {
		t.Fatalf("count charges: %v", err)
	}
	return l
}

func wantPaidOnce(t *testing.T, l ledger) {
	t.Helper()
	if l.invoices != 1 || l.status != "paid" || l.charges != 1 || l.payment == nil || l.chargeID == nil || *l.payment != *l.chargeID {
		t.Fatalf("books = %d invoice(s) %q payment %v, %d charge(s) %v; want one paid invoice recording the one charge",
			l.invoices, l.status, deref(l.payment), l.charges, deref(l.chargeID))
	}
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

func TestSettlePaysOnceAndIsIdempotent(t *testing.T) {
	pool := db.NewDB(t, 10)
	a := closedAuction(t, pool, 1100)
	s := newSettler(pool, provider(t, pool, 0), 3)
	if out, err := s.Settle(t.Context(), a); err != nil || out != settlement.Paid {
		t.Fatalf("settle = %v, %v; want paid", out, err)
	}
	for range 3 { // redeliveries
		if out, err := s.Settle(t.Context(), a); err != nil || out != settlement.AlreadySettled {
			t.Fatalf("resettle = %v, %v; want already_settled", out, err)
		}
	}
	wantPaidOnce(t, books(t, pool, a))
}

func TestSettleRetriesUnknownOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		rolls []float64
	}{
		{"failure before charging", []float64{failBefore, failBefore}},
		{"lost response after charging", []float64{lostAfter}},
		{"hang past the client timeout", []float64{hangAfter}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := db.NewDB(t, 10)
			a := closedAuction(t, pool, 1100)
			s := newSettler(pool, provider(t, pool, 2*time.Second, tc.rolls...), 5)
			if out, err := s.Settle(t.Context(), a); err != nil || out != settlement.Paid {
				t.Fatalf("settle = %v, %v; want paid", out, err)
			}
			wantPaidOnce(t, books(t, pool, a))
		})
	}
}

func TestSettleDeclined(t *testing.T) {
	pool := db.NewDB(t, 10)
	a := closedAuction(t, pool, 1113) // ends in 13: the simulator declines it
	s := newSettler(pool, provider(t, pool, 0), 3)
	if out, err := s.Settle(t.Context(), a); err != nil || out != settlement.Declined {
		t.Fatalf("settle = %v, %v; want declined", out, err)
	}
	if out, err := s.Settle(t.Context(), a); err != nil || out != settlement.AlreadySettled {
		t.Fatalf("resettle = %v, %v; want already_settled", out, err)
	}
	if l := books(t, pool, a); l.invoices != 1 || l.status != "failed" || l.charges != 0 {
		t.Fatalf("books = %+v; want one failed invoice and no charge", l)
	}
}

// Giving up must not claim the invoice failed: after lost responses a
// charge exists. A later settle (the dead-letter replay) finds it through
// the same key and records it; the customer is still charged once.
func TestGiveUpLeavesInvoicePendingUntilReplay(t *testing.T) {
	pool := db.NewDB(t, 10)
	a := closedAuction(t, pool, 1100)
	url := provider(t, pool, 0, lostAfter, lostAfter, lostAfter)
	s := newSettler(pool, url, 3)
	if _, err := s.Settle(t.Context(), a); !errors.Is(err, settlement.ErrGaveUp) {
		t.Fatalf("settle = %v; want ErrGaveUp", err)
	}
	if l := books(t, pool, a); l.status != "pending" || l.charges != 1 {
		t.Fatalf("after giving up: books = %+v; want a pending invoice and the one charge that did happen", l)
	}
	if out, err := s.Settle(t.Context(), a); err != nil || out != settlement.Paid {
		t.Fatalf("replay = %v, %v; want paid", out, err)
	}
	wantPaidOnce(t, books(t, pool, a))
}

func TestSettleNoWinner(t *testing.T) {
	pool := db.NewDB(t, 10)
	a := closedAuction(t, pool, 0)
	s := newSettler(pool, provider(t, pool, 0), 3)
	if out, err := s.Settle(t.Context(), a); err != nil || out != settlement.NoWinner {
		t.Fatalf("settle = %v, %v; want no_winner", out, err)
	}
	if l := books(t, pool, a); l.invoices != 0 || l.charges != 0 {
		t.Fatalf("books = %+v; want nothing", l)
	}
}

func TestSettleOpenAuctionIsRejected(t *testing.T) {
	pool := db.NewDB(t, 10)
	a := closedAuction(t, pool, 0)
	var open int64
	if err := pool.QueryRow(t.Context(), `
		INSERT INTO auctions (item_id, start_at, end_at, starting_price, min_increment)
		SELECT item_id, start_at, clock_timestamp() + interval '1 hour', 100, 1 FROM auctions WHERE id = $1
		RETURNING id`, a).Scan(&open); err != nil {
		t.Fatalf("seed open auction: %v", err)
	}
	s := newSettler(pool, provider(t, pool, 0), 3)
	if _, err := s.Settle(t.Context(), open); !errors.Is(err, settlement.ErrRejected) {
		t.Fatalf("settle open auction = %v; want ErrRejected", err)
	}
}

// Many settlers on one auction at once, through a provider that fails,
// loses responses and hangs at random: one invoice, one charge, and the
// invoice records that charge.
func TestConcurrentSettlesChargeOnce(t *testing.T) {
	pool := db.NewDB(t, 30)
	a := closedAuction(t, pool, 1100)
	rolls := make([]float64, 60)
	for i := range rolls {
		rolls[i] = []float64{failBefore, lostAfter, hangAfter, 0.99}[i%4]
	}
	url := provider(t, pool, 300*time.Millisecond, rolls...)
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			s := newSettler(pool, url, 20)
			if out, err := s.Settle(t.Context(), a); err != nil || (out != settlement.Paid && out != settlement.AlreadySettled) {
				t.Errorf("settle = %v, %v", out, err)
			}
		})
	}
	wg.Wait()
	wantPaidOnce(t, books(t, pool, a))
}

type countingPayer struct {
	mu    sync.Mutex
	calls []time.Time
	fail  int // this many calls fail with an unknown outcome first
}

func (p *countingPayer) Charge(context.Context, string, int64, int64) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, time.Now())
	if len(p.calls) <= p.fail {
		return "", settlement.ErrUnknownOutcome
	}
	return "ch_x", nil
}

// While the breaker is open, no call reaches the provider; settlement
// waits instead, and resumes with a trial call once it half-opens.
func TestOpenBreakerPausesCalls(t *testing.T) {
	pool := db.NewDB(t, 10)
	a := closedAuction(t, pool, 1100)
	pay := &countingPayer{fail: 2}
	const openFor = 150 * time.Millisecond
	b := breaker.New(1, openFor, time.Now, nil)
	s := settlement.NewSettler(pool, pay, b, 5, time.Millisecond, 2*time.Millisecond)
	if out, err := s.Settle(t.Context(), a); err != nil || out != settlement.Paid {
		t.Fatalf("settle = %v, %v; want paid", out, err)
	}
	if len(pay.calls) != 3 {
		t.Fatalf("%d calls, want 3 (fail, trial fails, trial succeeds)", len(pay.calls))
	}
	for i := 1; i < len(pay.calls); i++ {
		if gap := pay.calls[i].Sub(pay.calls[i-1]); gap < openFor {
			t.Errorf("call %d came %s after the previous one, while the breaker was open for %s", i, gap, openFor)
		}
	}
}
