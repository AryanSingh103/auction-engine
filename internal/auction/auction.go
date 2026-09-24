// Package auction holds the auction domain: the bidding rules and the bid
// path that applies them to Postgres.
package auction

import (
	"errors"
	"fmt"
	"math"
	"time"
)

// Status is an auction's lifecycle state as stored in auctions.status.
type Status string

// Auction statuses. Closing is done by the closer worker (milestone 5).
const (
	StatusOpen   Status = "open"
	StatusClosed Status = "closed"
)

// Auction is the state of one auction as the bid path sees it.
type Auction struct {
	ID            int64
	ItemID        int64
	StartAt       time.Time
	EndAt         time.Time
	StartingPrice int64 // cents
	MinIncrement  int64 // cents
	Status        Status
	// Head is the current highest accepted bid, or nil before the first one.
	Head *Head
}

// Head is the auction's current highest accepted bid.
type Head struct {
	BidID  int64
	UserID int64
	Price  int64 // cents
}

// MinimumBid is the smallest amount the next bid may be: the starting price
// for the first bid, then the current price plus the minimum increment.
//
// If price+increment would overflow int64 it returns math.MaxInt64 rather
// than wrapping to a negative number, which would accept any bid. (Postgres
// raises an error on the same overflow in the guard trigger.)
func (a Auction) MinimumBid() int64 {
	if a.Head == nil {
		return a.StartingPrice
	}
	if a.Head.Price > math.MaxInt64-a.MinIncrement {
		return math.MaxInt64
	}
	return a.Head.Price + a.MinIncrement
}

// Sentinel errors for rejected bids. The database guard trigger raises the
// same conditions with SQLSTATEs AE001 to AE006 (see migrations/00006).
var (
	ErrAuctionNotFound   = errors.New("auction not found")
	ErrAuctionNotOpen    = errors.New("auction is not open")
	ErrAuctionNotStarted = errors.New("auction has not started")
	ErrAuctionEnded      = errors.New("auction has ended")
	ErrSelfOutbid        = errors.New("bidder already leads the auction")
	ErrBidTooLow         = errors.New("bid is below the minimum")
	ErrUnknownUser       = errors.New("unknown user")
	ErrInvalidAmount     = errors.New("bid amount must be positive")
	// ErrRejectedByDatabaseGuard marks a rejection that the Go rules missed
	// and the database guard trigger caught. It is always wrapped together
	// with the specific reason (e.g. ErrAuctionEnded). In correct operation
	// it never appears; tests assert that, so a regression in the Go bid
	// path cannot hide behind the backstop.
	ErrRejectedByDatabaseGuard = errors.New("rejected by database guard")
	// ErrIdempotencyConflict means the idempotency key was already used by
	// this user for a different bid (different auction or amount).
	ErrIdempotencyConflict = errors.New("idempotency key reused with a different request")
)

// BidTooLowError reports the minimum a rejected bid needed to reach. It
// matches ErrBidTooLow with errors.Is.
type BidTooLowError struct {
	Amount  int64
	Minimum int64
}

func (e *BidTooLowError) Error() string {
	return fmt.Sprintf("bid %d is below the minimum %d", e.Amount, e.Minimum)
}

// Is makes errors.Is(err, ErrBidTooLow) true for a *BidTooLowError.
func (e *BidTooLowError) Is(target error) bool { return target == ErrBidTooLow }

// validateBid decides whether userID may bid amount on a at time now. It must
// be called with the auction row locked, and now must be read after the lock
// was acquired, so the decision cannot be invalidated before the bid is
// written. The checks run in the same order as the database guard trigger.
func validateBid(a Auction, userID, amount int64, now time.Time) error {
	switch {
	case amount <= 0:
		return ErrInvalidAmount
	case a.Status != StatusOpen:
		return ErrAuctionNotOpen
	case now.Before(a.StartAt):
		return ErrAuctionNotStarted
	case !now.Before(a.EndAt): // the window is [StartAt, EndAt)
		return ErrAuctionEnded
	case a.Head != nil && a.Head.UserID == userID:
		return ErrSelfOutbid
	case amount < a.MinimumBid():
		return &BidTooLowError{Amount: amount, Minimum: a.MinimumBid()}
	}
	return nil
}
