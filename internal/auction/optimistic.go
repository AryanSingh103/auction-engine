package auction

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Locking selects the bid path's concurrency strategy.
type Locking string

// Locking strategies. See docs/decisions/007 (pessimistic, the default) and
// docs/decisions/014 (optimistic, kept for benchmarking).
const (
	LockingPessimistic Locking = "pessimistic"
	LockingOptimistic  Locking = "optimistic"
)

// WithLocking selects the concurrency strategy for PlaceBid.
func WithLocking(l Locking) Option {
	return func(s *Service) { s.locking = l }
}

// ErrContention means the optimistic strategy lost the race for the
// auction head on every attempt. The bid was not written; retrying with the
// same idempotency key is safe.
var ErrContention = errors.New("too much contention on this auction; retry")

// maxOptimisticAttempts bounds the retry loop so a hot auction degrades
// into fast failures instead of unbounded latency.
const maxOptimisticAttempts = 10

// Full-jitter backoff between optimistic attempts: sleep a random duration
// in [0, min(backoffCap, backoffBase << attempt)). Randomness spreads
// retries out so conflicting bidders do not collide again in lockstep.
const (
	backoffBase = time.Millisecond
	backoffCap  = 20 * time.Millisecond
)

// errOptimisticConflict marks an attempt that read a head which another bid
// replaced before this one could extend it.
var errOptimisticConflict = errors.New("auction head changed during the transaction")

// placeBidOptimistic reads the auction without locking it, validates, and
// tries to extend the head it saw. If another bid got there first, the
// schema rejects the write (the guard trigger's stale-head check, AE004, or
// the no-fork unique constraint), and the whole transaction is retried with
// fresh state.
//
// It is "optimistic read, lock at write", not lock-free: the guard trigger
// still takes the auction row lock during the INSERT. What changes is how
// long the lock is held, from the INSERT to COMMIT instead of the whole
// transaction. See docs/decisions/014.
func (s *Service) placeBidOptimistic(ctx context.Context, req PlaceBidRequest) (Bid, bool, error) {
	for attempt := 1; ; attempt++ {
		bid, replayed, err := s.placeBidOptimisticTx(ctx, req)
		if !errors.Is(err, errOptimisticConflict) {
			return bid, replayed, err
		}
		s.obs.OptimisticConflict()
		if attempt >= maxOptimisticAttempts {
			return Bid{}, false, ErrContention
		}
		if err := sleepCtx(ctx, backoff(attempt)); err != nil {
			return Bid{}, false, err
		}
	}
}

func (s *Service) placeBidOptimisticTx(ctx context.Context, req PlaceBidRequest) (bid Bid, replayed bool, err error) {
	txStart := time.Now()
	defer func() { s.obs.BidTransaction(time.Since(txStart)) }()

	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE id = $1)`, req.UserID).Scan(&exists); err != nil {
			return fmt.Errorf("check user: %w", err)
		}
		if !exists {
			return ErrUnknownUser
		}

		if prior, found, err := findBidByKey(ctx, tx, req.UserID, req.IdempotencyKey); err != nil {
			return err
		} else if found {
			return replayOrConflict(prior, req, &bid, &replayed)
		}

		// No lock: this is a snapshot that may already be stale.
		a, err := scanAuction(tx.QueryRow(ctx, auctionColumns+` WHERE id = $1`, req.AuctionID))
		if err != nil {
			return err
		}
		var now time.Time
		if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
			return fmt.Errorf("read clock: %w", err)
		}

		if err := validateBid(a, req.UserID, req.Amount, now); err != nil {
			// Without the lock, another copy of this same request may have
			// committed after the key lookup above, making this attempt look
			// like a self-outbid or a too-low bid. Re-check the key (this
			// statement sees everything committed so far) before rejecting.
			if prior, found, lerr := findBidByKey(ctx, tx, req.UserID, req.IdempotencyKey); lerr != nil {
				return lerr
			} else if found {
				return replayOrConflict(prior, req, &bid, &replayed)
			}
			return err
		}

		bid, err = writeBid(ctx, tx, req, a, s.obs.LockWait)
		return err
	})
	switch {
	case err == nil:
		return bid, replayed, nil
	case isOptimisticConflict(err):
		return Bid{}, false, fmt.Errorf("%w: %w", errOptimisticConflict, err)
	default:
		return Bid{}, false, mapDBError(err)
	}
}

func replayOrConflict(prior Bid, req PlaceBidRequest, bid *Bid, replayed *bool) error {
	if prior.AuctionID != req.AuctionID || prior.Amount != req.Amount {
		return ErrIdempotencyConflict
	}
	*bid, *replayed = prior, true
	return nil
}

// isOptimisticConflict reports whether err means "the head moved": the
// guard trigger's stale-head error or the chain's no-fork constraint.
func isOptimisticConflict(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == "AE004" || (pgErr.Code == "23505" && pgErr.ConstraintName == "bids_chain_no_fork")
}

func backoff(attempt int) time.Duration {
	ceiling := min(backoffCap, backoffBase<<attempt)
	return rand.N(ceiling) //nolint:gosec // jitter spreads retries; it needs no unpredictability
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
