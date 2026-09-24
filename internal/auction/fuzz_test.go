package auction

import (
	"errors"
	"testing"
	"time"
)

// FuzzBidSequence replays arbitrary sequences of bids (random bidders and
// amounts, in random order) through validateBid, applying each accepted bid
// the way the bid path does, and checks the invariants on the outcome:
//
//   - invariant 3: every accepted bid beats the previous accepted one by at
//     least MinIncrement (and the first is at least StartingPrice)
//   - invariant 2: the final leader holds the highest accepted amount
//   - nobody outbids themselves
//   - every rejection is justified: "too low" really was below the minimum
//     at that moment, and "self-outbid" really was the leader
//
// `go test` runs only the seed corpus below; `go test -fuzz=FuzzBidSequence
// ./internal/auction` explores new inputs.
func FuzzBidSequence(f *testing.F) {
	f.Add([]byte{0, 10, 1, 20, 2, 30})                    // steady raises
	f.Add([]byte{0, 200, 1, 5, 2, 5, 3, 250})             // big first bid, then low ones
	f.Add([]byte{0, 10, 0, 50, 1, 60, 1, 90})             // self-outbid attempts
	f.Add([]byte{3, 0, 3, 0, 2, 1, 2, 1, 1, 255, 0, 255}) // ties and extremes

	f.Fuzz(func(t *testing.T, data []byte) {
		a := Auction{
			StartAt: start, EndAt: end,
			StartingPrice: 1000, MinIncrement: 100,
			Status: StatusOpen,
		}
		now := start.Add(time.Minute)

		var accepted []int64 // amounts, in acceptance order
		var lastUser int64 = -1
		var nextBidID int64 = 1

		// Each pair of bytes is one bid: bidder (4 users) and amount
		// (0..255 steps of 50 cents on top of 900, so sequences mix too-low,
		// exact-minimum and large bids).
		for i := 0; i+1 < len(data); i += 2 {
			user := int64(data[i] % 4)
			amount := 900 + int64(data[i+1])*50
			minBefore := a.MinimumBid()

			err := validateBid(a, user, amount, now)
			switch {
			case err == nil:
				if amount < minBefore {
					t.Fatalf("accepted %d below minimum %d", amount, minBefore)
				}
				if user == lastUser {
					t.Fatalf("accepted self-outbid by user %d", user)
				}
				a.Head = &Head{BidID: nextBidID, UserID: user, Price: amount}
				nextBidID++
				accepted = append(accepted, amount)
				lastUser = user
			case errors.Is(err, ErrBidTooLow):
				if amount >= minBefore {
					t.Fatalf("rejected %d as too low, but minimum was %d", amount, minBefore)
				}
			case errors.Is(err, ErrSelfOutbid):
				if user != lastUser {
					t.Fatalf("rejected user %d as self-outbid, but leader was %d", user, lastUser)
				}
			default:
				t.Fatalf("unexpected rejection for an open auction: %v", err)
			}
		}

		for i, amt := range accepted {
			switch {
			case i == 0 && amt < a.StartingPrice:
				t.Fatalf("first accepted bid %d below starting price", amt)
			case i > 0 && amt < accepted[i-1]+a.MinIncrement:
				t.Fatalf("accepted bid %d does not beat %d by the increment", amt, accepted[i-1])
			}
		}
		if len(accepted) > 0 {
			highest := accepted[len(accepted)-1]
			for _, amt := range accepted {
				if amt > highest {
					t.Fatalf("leader holds %d but %d was accepted earlier", highest, amt)
				}
			}
			if a.Head.Price != highest {
				t.Fatalf("head price %d, want highest accepted %d", a.Head.Price, highest)
			}
		}
	})
}
