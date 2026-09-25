package auction

import (
	"context"
	"errors"
	"time"
)

// Outcome classifies how a PlaceBid call ended, for metrics.
type Outcome string

// Every possible outcome. Metrics pre-create a series for each, so rates
// read as 0 rather than "no data" before the first occurrence.
const (
	OutcomeAccepted            Outcome = "accepted"
	OutcomeReplayed            Outcome = "replayed"
	OutcomeTooLow              Outcome = "too_low"
	OutcomeSelfOutbid          Outcome = "self_outbid"
	OutcomeEnded               Outcome = "ended"
	OutcomeNotOpen             Outcome = "not_open"
	OutcomeNotStarted          Outcome = "not_started"
	OutcomeIdempotencyConflict Outcome = "idempotency_conflict"
	OutcomeUnknownUser         Outcome = "unknown_user"
	OutcomeNotFound            Outcome = "not_found"
	OutcomeInvalidAmount       Outcome = "invalid_amount"
	// OutcomeGuardBoundary: the database guard rejected a bid at end_at,
	// the one expected guard case (see IsExpectedGuardRejection).
	OutcomeGuardBoundary Outcome = "guard_boundary"
	// OutcomeGuardBug: the database guard caught what Go validation
	// missed. Should stay at zero; alert on it.
	OutcomeGuardBug Outcome = "guard_bug"
	OutcomeTimeout  Outcome = "timeout"
	// OutcomeContention: the optimistic strategy ran out of attempts.
	OutcomeContention Outcome = "contention"
	OutcomeError      Outcome = "error"
)

// AllOutcomes lists every Outcome.
var AllOutcomes = []Outcome{
	OutcomeAccepted, OutcomeReplayed, OutcomeTooLow, OutcomeSelfOutbid, OutcomeEnded,
	OutcomeNotOpen, OutcomeNotStarted, OutcomeIdempotencyConflict, OutcomeUnknownUser,
	OutcomeNotFound, OutcomeInvalidAmount, OutcomeGuardBoundary, OutcomeGuardBug,
	OutcomeTimeout, OutcomeContention, OutcomeError,
}

// OutcomeOf maps a PlaceBid result to its Outcome.
func OutcomeOf(err error, replayed bool) Outcome {
	// Guard classifications first: a guard error also matches its reason.
	switch {
	case err == nil && replayed:
		return OutcomeReplayed
	case err == nil:
		return OutcomeAccepted
	case IsExpectedGuardRejection(err):
		return OutcomeGuardBoundary
	case errors.Is(err, ErrRejectedByDatabaseGuard):
		return OutcomeGuardBug
	}
	for _, m := range []struct {
		err     error
		outcome Outcome
	}{
		{ErrBidTooLow, OutcomeTooLow},
		{ErrSelfOutbid, OutcomeSelfOutbid},
		{ErrAuctionEnded, OutcomeEnded},
		{ErrAuctionNotOpen, OutcomeNotOpen},
		{ErrAuctionNotStarted, OutcomeNotStarted},
		{ErrIdempotencyConflict, OutcomeIdempotencyConflict},
		{ErrUnknownUser, OutcomeUnknownUser},
		{ErrAuctionNotFound, OutcomeNotFound},
		{ErrInvalidAmount, OutcomeInvalidAmount},
		{ErrContention, OutcomeContention},
		{context.DeadlineExceeded, OutcomeTimeout},
		{context.Canceled, OutcomeTimeout},
	} {
		if errors.Is(err, m.err) {
			return m.outcome
		}
	}
	return OutcomeError
}

// Observer receives measurements from the bid path. It keeps this package
// free of any metrics library: internal/metrics implements it for
// Prometheus, and tests can record calls directly. Implementations must be
// safe for concurrent use.
type Observer interface {
	// BidOutcome is called once per PlaceBid call.
	BidOutcome(Outcome)
	// LockWait is the time spent acquiring the auction row lock, the direct
	// measure of contention on a hot auction.
	LockWait(time.Duration)
	// BidTransaction is the duration of one bid transaction attempt, from
	// BEGIN to COMMIT or ROLLBACK.
	BidTransaction(time.Duration)
	// OptimisticConflict is called each time an optimistic attempt finds
	// that the auction head moved and must be retried (or gives up).
	OptimisticConflict()
}

type nopObserver struct{}

func (nopObserver) BidOutcome(Outcome)           {}
func (nopObserver) LockWait(time.Duration)       {}
func (nopObserver) BidTransaction(time.Duration) {}
func (nopObserver) OptimisticConflict()          {}

// Cache is a read cache of auction state (internal/cache implements it).
// It is used only by GetAuction; the bid path never reads it.
type Cache interface {
	Get(ctx context.Context, id int64) (Auction, bool, error)
	Put(ctx context.Context, a Auction) error
}

// WithCache serves GetAuction from c and refreshes c after every accepted
// bid.
func WithCache(c Cache) Option {
	return func(s *Service) { s.cache = c }
}

// Option configures a Service.
type Option func(*Service)

// WithObserver sends bid-path measurements to o.
func WithObserver(o Observer) Option {
	return func(s *Service) { s.obs = o }
}
