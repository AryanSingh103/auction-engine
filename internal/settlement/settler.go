package settlement

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AryanSingh103/auction-engine/internal/breaker"
)

// ErrGaveUp: every attempt ended with ErrUnknownOutcome. The invoice stays
// pending, because a charge may exist (a lost response), and the event
// goes to the dead-letter topic to be replayed.
var ErrGaveUp = errors.New("payment outcome still unknown after all attempts")

// Outcome is what settling one closed auction did.
type Outcome int

const (
	Paid           Outcome = iota // charged now
	Declined                      // provider declined; invoice failed
	AlreadySettled                // paid or failed before (a redelivery)
	NoWinner                      // closed without bids: nothing to charge
)

func (o Outcome) String() string {
	return [...]string{"paid", "declined", "already_settled", "no_winner"}[o]
}

// Observer receives settlement events, for metrics.
type Observer interface {
	// Settled reports one Settle call: an Outcome's name, or "gave_up",
	// "rejected" or "error".
	Settled(result string)
	// PaymentAttempt reports one call to the provider: "ok", "declined",
	// "rejected" or "unknown".
	PaymentAttempt(result string)
	// DeadLettered reports an event sent to the dead-letter topic.
	DeadLettered()
}

type nopObserver struct{}

func (nopObserver) Settled(string)        {}
func (nopObserver) PaymentAttempt(string) {}
func (nopObserver) DeadLettered()         {}

// Settler settles closed auctions.
type Settler struct {
	pool        *pgxpool.Pool
	pay         Payer
	breaker     *breaker.Breaker
	maxAttempts int
	backoffBase time.Duration
	backoffCap  time.Duration
	obs         Observer
}

// NewSettler returns a settler that makes up to maxAttempts payment
// attempts per invoice, with full-jitter exponential backoff between them.
// obs may be nil.
func NewSettler(pool *pgxpool.Pool, pay Payer, b *breaker.Breaker, maxAttempts int, backoffBase, backoffCap time.Duration, obs Observer) *Settler {
	if obs == nil {
		obs = nopObserver{}
	}
	return &Settler{pool: pool, pay: pay, breaker: b, maxAttempts: maxAttempts, backoffBase: backoffBase, backoffCap: backoffCap, obs: obs}
}

type invoice struct {
	id, winner, amount int64
	status             string
}

// Settle charges the winner of a closed auction, once. It is idempotent:
// run it any number of times, concurrently or not, for the same auction
// and the winner is charged once. That is the whole contract with the
// at-least-once event stream.
//
// Returned errors: ErrGaveUp and ErrRejected are final for this delivery
// (dead-letter it); a context error means stop; anything else (a database
// error) is worth retrying the whole call.
func (s *Settler) Settle(ctx context.Context, auctionID int64) (out Outcome, err error) {
	defer func() {
		switch {
		case err == nil:
			s.obs.Settled(out.String())
		case errors.Is(err, ErrGaveUp):
			s.obs.Settled("gave_up")
		case errors.Is(err, ErrRejected):
			s.obs.Settled("rejected")
		case ctx.Err() == nil:
			s.obs.Settled("error")
		}
	}()
	inv, found, err := s.ensureInvoice(ctx, auctionID)
	if err != nil {
		return 0, err
	}
	if !found {
		return NoWinner, nil
	}
	if inv.status != "pending" {
		return AlreadySettled, nil
	}

	// The key is the invoice, not the event or the attempt: every attempt,
	// every redelivery and every settler instance uses the same one.
	chargeID, err := s.charge(ctx, "invoice-"+strconv.FormatInt(inv.id, 10), inv.amount, inv.winner)
	switch {
	case err == nil:
		return Paid, s.markPaid(ctx, inv.id, chargeID)
	case errors.Is(err, ErrDeclined):
		return Declined, s.markDeclined(ctx, inv.id)
	default:
		return 0, err
	}
}

// ensureInvoice creates the auction's invoice from the closed auction row
// (the source of truth, not the event payload), or finds the one that
// exists. found is false if the auction closed without a winner.
func (s *Settler) ensureInvoice(ctx context.Context, auctionID int64) (invoice, bool, error) {
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO invoices (auction_id, bid_id, winner_id, amount)
		SELECT id, current_bid_id, current_leader_id, current_price
		FROM auctions
		WHERE id = $1 AND status = 'closed' AND current_bid_id IS NOT NULL
		ON CONFLICT (auction_id) DO NOTHING`, auctionID); err != nil {
		return invoice{}, false, fmt.Errorf("create invoice: %w", permanentIfRefused(err))
	}
	var inv invoice
	err := s.pool.QueryRow(ctx, `SELECT id, winner_id, amount, status FROM invoices WHERE auction_id = $1`, auctionID).
		Scan(&inv.id, &inv.winner, &inv.amount, &inv.status)
	if err == nil {
		return inv, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return invoice{}, false, fmt.Errorf("read invoice: %w", err)
	}

	// No invoice: fine only for a closed auction without bids. The event is
	// published after the close commits, so "not closed" is a bug.
	var status string
	var head *int64
	err = s.pool.QueryRow(ctx, `SELECT status, current_bid_id FROM auctions WHERE id = $1`, auctionID).Scan(&status, &head)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return invoice{}, false, fmt.Errorf("%w: auction %d does not exist", ErrRejected, auctionID)
	case err != nil:
		return invoice{}, false, fmt.Errorf("read auction: %w", err)
	case status != "closed" || head != nil:
		return invoice{}, false, fmt.Errorf("%w: auction %d is %s with head %v but has no invoice", ErrRejected, auctionID, status, head)
	}
	return invoice{}, false, nil
}

// charge calls the provider until it gives a definite answer, the attempts
// run out, or ctx ends. While the breaker is open it waits without using
// up attempts: the provider is known to be down, and the right response is
// to pause settlement, not to dead-letter every event.
func (s *Settler) charge(ctx context.Context, key string, amount, customer int64) (string, error) {
	for attempt := 1; ; {
		if err := s.breaker.Allow(); err != nil {
			if err := sleep(ctx, max(s.breaker.RetryAfter(), s.backoffBase)); err != nil {
				return "", err
			}
			continue
		}
		id, err := s.pay.Charge(ctx, key, amount, customer)
		unknown := errors.Is(err, ErrUnknownOutcome)
		switch {
		case err == nil:
			s.obs.PaymentAttempt("ok")
		case errors.Is(err, ErrDeclined):
			s.obs.PaymentAttempt("declined")
		case unknown:
			s.obs.PaymentAttempt("unknown")
		default:
			s.obs.PaymentAttempt("rejected")
		}
		// Our own shutdown is not the provider's fault.
		if ctx.Err() == nil {
			s.breaker.Record(!unknown)
		}
		if !unknown {
			return id, err
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if attempt >= s.maxAttempts {
			return "", fmt.Errorf("%w (%d attempts): %w", ErrGaveUp, attempt, err)
		}
		if err := sleep(ctx, s.backoff(attempt)); err != nil {
			return "", err
		}
		attempt++
	}
}

// backoff is "full jitter": uniform in [0, min(cap, base*2^attempt)).
// Randomness spreads out settlers that failed together, so they do not
// retry in lockstep and hit a recovering provider all at once.
func (s *Settler) backoff(attempt int) time.Duration {
	ceiling := s.backoffCap
	if attempt < 30 { // avoid overflowing the shift
		ceiling = min(s.backoffCap, s.backoffBase<<attempt)
	}
	return rand.N(ceiling) + 1 //nolint:gosec // jitter spreads retries; it needs no unpredictability
}

// permanentIfRefused marks a database error as ErrRejected when retrying
// cannot help: an integrity violation (SQLSTATE class 23) or one of the
// schema's own guards (AE...). Retrying those forever would block the
// partition with nothing in the dead-letter topic (found by the M4
// review). Anything else (a lost connection, a timeout) stays transient.
func permanentIfRefused(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && (strings.HasPrefix(pgErr.Code, "23") || strings.HasPrefix(pgErr.Code, "AE")) {
		return fmt.Errorf("%w: %w", ErrRejected, err)
	}
	return err
}

func (s *Settler) markPaid(ctx context.Context, id int64, chargeID string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE invoices SET status = 'paid', payment_id = $2, settled_at = clock_timestamp()
		WHERE id = $1 AND status = 'pending'`, id, chargeID)
	if err != nil {
		return fmt.Errorf("mark invoice %d paid: %w", id, permanentIfRefused(err))
	}
	if tag.RowsAffected() == 0 {
		// Another settler got there first. Same key, same charge: it must
		// have recorded the same payment.
		var status string
		var recorded *string
		if err := s.pool.QueryRow(ctx, `SELECT status, payment_id FROM invoices WHERE id = $1`, id).Scan(&status, &recorded); err != nil {
			return fmt.Errorf("re-read invoice %d: %w", id, err)
		}
		if status != "paid" || recorded == nil || *recorded != chargeID {
			return fmt.Errorf("%w: invoice %d is %s with payment %v, but provider charged %s", ErrRejected, id, status, recorded, chargeID)
		}
	}
	return nil
}

func (s *Settler) markDeclined(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE invoices SET status = 'failed', failure = 'declined by payment provider', settled_at = clock_timestamp()
		WHERE id = $1 AND status = 'pending'`, id)
	if err != nil {
		return fmt.Errorf("mark invoice %d failed: %w", id, permanentIfRefused(err))
	}
	if tag.RowsAffected() == 0 {
		// The provider replays a key's result, so another settler must
		// have been declined too.
		var status string
		if err := s.pool.QueryRow(ctx, `SELECT status FROM invoices WHERE id = $1`, id).Scan(&status); err != nil {
			return fmt.Errorf("re-read invoice %d: %w", id, err)
		}
		if status != "failed" {
			return fmt.Errorf("%w: invoice %d is %s, but the provider declined it", ErrRejected, id, status)
		}
	}
	return nil
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
