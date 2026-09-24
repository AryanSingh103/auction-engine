package auction

import (
	"errors"
	"math"
	"testing"
	"time"
)

var (
	t0    = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	start = t0
	end   = t0.Add(time.Hour)
)

func openAuction(head *Head) Auction {
	return Auction{
		ID: 1, ItemID: 1,
		StartAt: start, EndAt: end,
		StartingPrice: 1000, MinIncrement: 100,
		Status: StatusOpen,
		Head:   head,
	}
}

func TestValidateBid(t *testing.T) {
	leader := &Head{BidID: 7, UserID: 42, Price: 1500}
	mid := start.Add(30 * time.Minute)

	tests := []struct {
		name    string
		auction Auction
		user    int64
		amount  int64
		now     time.Time
		want    error // nil means accepted
	}{
		{"first bid at starting price", openAuction(nil), 1, 1000, mid, nil},
		{"first bid below starting price", openAuction(nil), 1, 999, mid, ErrBidTooLow},
		{"raise by exactly the increment", openAuction(leader), 1, 1600, mid, nil},
		{"raise one cent short", openAuction(leader), 1, 1599, mid, ErrBidTooLow},
		{"equal to current price", openAuction(leader), 1, 1500, mid, ErrBidTooLow},
		{"leader outbids self", openAuction(leader), 42, 9999, mid, ErrSelfOutbid},
		{"zero amount", openAuction(nil), 1, 0, mid, ErrInvalidAmount},
		{"negative amount", openAuction(nil), 1, -1000, mid, ErrInvalidAmount},
		{"closed status", func() Auction { a := openAuction(nil); a.Status = StatusClosed; return a }(), 1, 1000, mid, ErrAuctionNotOpen},
		{"before start", openAuction(nil), 1, 1000, start.Add(-time.Nanosecond), ErrAuctionNotStarted},
		{"exactly at start", openAuction(nil), 1, 1000, start, nil},
		{"one nanosecond before end", openAuction(nil), 1, 1000, end.Add(-time.Nanosecond), nil},
		{"exactly at end", openAuction(nil), 1, 1000, end, ErrAuctionEnded},
		{"after end", openAuction(nil), 1, 1000, end.Add(time.Second), ErrAuctionEnded},
		// When several rules fail, the first in the documented order wins,
		// matching the database trigger's order.
		{"ended beats self-outbid", openAuction(leader), 42, 1, end, ErrAuctionEnded},
		{"self-outbid beats too low", openAuction(leader), 42, 1, mid, ErrSelfOutbid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateBid(tt.auction, tt.user, tt.amount, tt.now)
			if !errors.Is(err, tt.want) || (tt.want == nil && err != nil) {
				t.Errorf("validateBid() = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestBidTooLowErrorReportsMinimum(t *testing.T) {
	err := validateBid(openAuction(&Head{UserID: 9, Price: 1500}), 1, 1550, start)

	var tooLow *BidTooLowError
	if !errors.As(err, &tooLow) {
		t.Fatalf("error = %v, want *BidTooLowError", err)
	}
	if tooLow.Minimum != 1600 || tooLow.Amount != 1550 {
		t.Errorf("got minimum %d amount %d, want 1600 and 1550", tooLow.Minimum, tooLow.Amount)
	}
}

func TestMinimumBidDoesNotOverflow(t *testing.T) {
	a := openAuction(&Head{UserID: 9, Price: math.MaxInt64 - 10})
	if got := a.MinimumBid(); got != math.MaxInt64 {
		t.Errorf("MinimumBid() = %d, want math.MaxInt64 (saturated, not wrapped)", got)
	}
	if err := validateBid(a, 1, 5000, start); !errors.Is(err, ErrBidTooLow) {
		t.Errorf("validateBid() = %v, want ErrBidTooLow instead of accepting after overflow", err)
	}
}
