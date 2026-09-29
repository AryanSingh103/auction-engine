package auction

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// PlaceBidInTx runs the pessimistic bid path's steps inside tx and leaves
// it open, so a test can hold an accepted-but-uncommitted bid (and its row
// lock) across the moment the auction ends. Tests only.
func PlaceBidInTx(ctx context.Context, tx pgx.Tx, req PlaceBidRequest) (Bid, error) {
	a, err := lockAuction(ctx, tx, req.AuctionID)
	if err != nil {
		return Bid{}, err
	}
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return Bid{}, fmt.Errorf("read clock: %w", err)
	}
	if err := validateBid(a, req.UserID, req.Amount, now); err != nil {
		return Bid{}, err
	}
	return writeBid(ctx, tx, req, a, nil)
}
