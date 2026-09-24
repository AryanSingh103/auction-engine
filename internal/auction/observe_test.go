package auction

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestOutcomeOf(t *testing.T) {
	guard := func(reason error) error { return fmt.Errorf("%w: %w", ErrRejectedByDatabaseGuard, reason) }
	tests := []struct {
		name     string
		err      error
		replayed bool
		want     Outcome
	}{
		{"accepted", nil, false, OutcomeAccepted},
		{"replayed", nil, true, OutcomeReplayed},
		{"too low", &BidTooLowError{Amount: 1, Minimum: 2}, false, OutcomeTooLow},
		{"wrapped too low", fmt.Errorf("x: %w", ErrBidTooLow), false, OutcomeTooLow},
		{"self outbid", ErrSelfOutbid, false, OutcomeSelfOutbid},
		{"ended", ErrAuctionEnded, false, OutcomeEnded},
		{"not open", ErrAuctionNotOpen, false, OutcomeNotOpen},
		{"not started", ErrAuctionNotStarted, false, OutcomeNotStarted},
		{"idempotency", ErrIdempotencyConflict, false, OutcomeIdempotencyConflict},
		{"unknown user", ErrUnknownUser, false, OutcomeUnknownUser},
		{"not found", ErrAuctionNotFound, false, OutcomeNotFound},
		{"invalid amount", ErrInvalidAmount, false, OutcomeInvalidAmount},
		// A guard rejection also matches its reason; the guard label wins.
		{"guard at end_at", guard(ErrAuctionEnded), false, OutcomeGuardBoundary},
		{"guard too low", guard(ErrBidTooLow), false, OutcomeGuardBug},
		{"deadline", fmt.Errorf("begin: %w", context.DeadlineExceeded), false, OutcomeTimeout},
		{"cancelled", context.Canceled, false, OutcomeTimeout},
		{"contention", ErrContention, false, OutcomeContention},
		{"other", errors.New("connection reset"), false, OutcomeError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := OutcomeOf(tt.err, tt.replayed); got != tt.want {
				t.Errorf("OutcomeOf() = %q, want %q", got, tt.want)
			}
		})
	}
}
