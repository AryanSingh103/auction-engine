package auction

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrAuctionNotEnded is returned when closing an auction before its end.
var ErrAuctionNotEnded = errors.New("auction has not ended yet")

// ClosedEvent is the auction_closed outbox payload. Winner fields are nil
// when the auction ended without bids. Settlement does not trust it for
// money: it reads the closed auction row, the source of truth.
type ClosedEvent struct {
	AuctionID int64     `json:"auction_id"`
	WinnerID  *int64    `json:"winner_id"`
	BidID     *int64    `json:"bid_id"`
	Amount    *int64    `json:"amount"`
	ClosedAt  time.Time `json:"closed_at"`
}

// CloseAuction closes an ended auction and writes its auction_closed event,
// in one transaction (the schema refuses either without the other: AE013,
// AE014). It reports whether this call closed it; closing an auction that
// is already closed is a no-op, so a retried close is safe. M5's closer is
// the normal caller.
//
// It takes the auction row lock first (the lock order, R14), so it
// serializes with the bid path: a bid either commits before the close and
// is part of the result, or runs after it and is rejected as closed.
func (s *Service) CloseAuction(ctx context.Context, id int64) (closed bool, err error) {
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		a, err := lockAuction(ctx, tx, id)
		if err != nil {
			return err
		}
		if a.Status == StatusClosed {
			return nil
		}
		// The database clock, read after the lock was granted (R1).
		var now time.Time
		if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
			return fmt.Errorf("read clock: %w", err)
		}
		if now.Before(a.EndAt) {
			return ErrAuctionNotEnded
		}

		if _, err := tx.Exec(ctx, `UPDATE auctions SET status = 'closed' WHERE id = $1`, id); err != nil {
			return fmt.Errorf("close auction: %w", err)
		}
		ev := ClosedEvent{AuctionID: id, ClosedAt: now}
		if a.Head != nil {
			ev.WinnerID, ev.BidID, ev.Amount = &a.Head.UserID, &a.Head.BidID, &a.Head.Price
		}
		payload, err := json.Marshal(ev)
		if err != nil {
			return fmt.Errorf("encode auction_closed event: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO outbox (auction_id, event_type, payload)
			VALUES ($1, 'auction_closed', $2)`, id, payload); err != nil {
			return fmt.Errorf("insert outbox event: %w", err)
		}
		closed = true
		return nil
	})
	if err != nil {
		return false, mapDBError(err)
	}
	if closed {
		// Best effort, as after a bid: readers see "closed" at once rather
		// than after the cache TTL.
		post, cancel := context.WithTimeout(context.WithoutCancel(ctx), postCommitTimeout)
		s.refreshCache(post, id)
		cancel()
	}
	return closed, nil
}
