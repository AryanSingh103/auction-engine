package auction

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Service runs the auction use cases against Postgres. It talks to pgx
// directly, with no repository interface: the behavior that matters here
// (row locks, constraints, triggers) only exists in the database, so it is
// tested against a real one rather than mocked.
type Service struct {
	pool *pgxpool.Pool
}

// NewService returns a Service using pool.
func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

// PlaceBidRequest is one bid attempt.
type PlaceBidRequest struct {
	AuctionID      int64
	UserID         int64
	Amount         int64 // cents
	IdempotencyKey string
}

// Bid is an accepted bid.
type Bid struct {
	ID        int64     `json:"id"`
	AuctionID int64     `json:"auction_id"`
	UserID    int64     `json:"user_id"`
	Amount    int64     `json:"amount"`
	PrevBidID *int64    `json:"prev_bid_id"`
	CreatedAt time.Time `json:"created_at"`
}

// PlaceBid validates and records a bid. replayed is true when the
// idempotency key had already been used for this exact bid, in which case
// the original bid is returned and nothing new is written.
//
// Everything happens in one transaction that holds the auction row lock
// from the first read of the auction until COMMIT, so no other bid (or the
// closer, from milestone 5) can change the auction in between. See
// docs/decisions/007.
func (s *Service) PlaceBid(ctx context.Context, req PlaceBidRequest) (bid Bid, replayed bool, err error) {
	bid, replayed, err = s.placeBidTx(ctx, req)
	if isConstraintViolation(err, "bids_idempotency") {
		// A concurrent request with the same key committed between our
		// lookup and our insert. Our transaction has rolled back; answer
		// with the winner's bid exactly as a later retry would get it.
		return s.replay(ctx, req)
	}
	return bid, replayed, err
}

func (s *Service) placeBidTx(ctx context.Context, req PlaceBidRequest) (bid Bid, replayed bool, err error) {
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// 1. Idempotency: a retry of an accepted bid returns the original.
		// This runs before taking the lock, so replays never queue behind
		// live bids.
		prior, found, err := findBidByKey(ctx, tx, req.UserID, req.IdempotencyKey)
		if err != nil {
			return err
		}
		if found {
			if prior.AuctionID != req.AuctionID || prior.Amount != req.Amount {
				return ErrIdempotencyConflict
			}
			bid, replayed = prior, true
			return nil
		}

		// 2. The bidder must exist. Checked before the lock so an unknown
		// user never holds up real bidders, and before validation so the
		// caller learns about identity before bid rules.
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE id = $1)`, req.UserID).Scan(&exists); err != nil {
			return fmt.Errorf("check user: %w", err)
		}
		if !exists {
			return ErrUnknownUser
		}

		// 3. Lock the auction row. Every other bid on this auction now waits
		// here until we commit or roll back.
		a, err := lockAuction(ctx, tx, req.AuctionID)
		if err != nil {
			return err
		}

		// 4. Read the database clock only now that the lock is held. A
		// timestamp taken before the lock wait could be from before the
		// auction ended even though we are deciding after it (R1).
		var now time.Time
		if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
			return fmt.Errorf("read clock: %w", err)
		}

		// 5. Validate while holding the lock.
		if err := validateBid(a, req.UserID, req.Amount, now); err != nil {
			return err
		}

		// 6. Write the bid, advance the auction head, and write the outbox
		// event, all in this transaction (invariant 6).
		var prev *int64
		if a.Head != nil {
			prev = &a.Head.BidID
		}
		bid = Bid{AuctionID: req.AuctionID, UserID: req.UserID, Amount: req.Amount, PrevBidID: prev}
		if err := tx.QueryRow(ctx, `
			INSERT INTO bids (auction_id, user_id, amount, prev_bid_id, idempotency_key)
			VALUES ($1, $2, $3, $4, $5)
			RETURNING id, created_at`,
			req.AuctionID, req.UserID, req.Amount, prev, req.IdempotencyKey,
		).Scan(&bid.ID, &bid.CreatedAt); err != nil {
			return fmt.Errorf("insert bid: %w", err)
		}

		if _, err := tx.Exec(ctx, `
			UPDATE auctions
			SET current_price = $2, current_leader_id = $3, current_bid_id = $4
			WHERE id = $1`,
			req.AuctionID, req.Amount, req.UserID, bid.ID); err != nil {
			return fmt.Errorf("advance auction head: %w", err)
		}

		payload, err := json.Marshal(bid)
		if err != nil {
			return fmt.Errorf("encode bid_placed event: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO outbox (auction_id, event_type, payload, bid_id)
			VALUES ($1, 'bid_placed', $2, $3)`,
			req.AuctionID, payload, bid.ID); err != nil {
			return fmt.Errorf("insert outbox event: %w", err)
		}
		return nil
	})
	if err != nil {
		return Bid{}, false, mapDBError(err)
	}
	return bid, replayed, nil
}

// replay returns the bid previously accepted under req's idempotency key.
func (s *Service) replay(ctx context.Context, req PlaceBidRequest) (Bid, bool, error) {
	prior, found, err := findBidByKey(ctx, s.pool, req.UserID, req.IdempotencyKey)
	if err != nil {
		return Bid{}, false, err
	}
	if !found {
		// The unique violation proved a row existed; it cannot vanish
		// because bids are never deleted.
		return Bid{}, false, errors.New("idempotency key conflicted but no bid found")
	}
	if prior.AuctionID != req.AuctionID || prior.Amount != req.Amount {
		return Bid{}, false, ErrIdempotencyConflict
	}
	return prior, true, nil
}

// GetAuction reads an auction without locking it.
func (s *Service) GetAuction(ctx context.Context, id int64) (Auction, error) {
	return scanAuction(s.pool.QueryRow(ctx, auctionColumns+` WHERE id = $1`, id))
}

// querier is satisfied by *pgxpool.Pool, *pgx.Conn and pgx.Tx.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func findBidByKey(ctx context.Context, q querier, userID int64, key string) (Bid, bool, error) {
	var b Bid
	err := q.QueryRow(ctx, `
		SELECT id, auction_id, user_id, amount, prev_bid_id, created_at
		FROM bids WHERE user_id = $1 AND idempotency_key = $2`,
		userID, key,
	).Scan(&b.ID, &b.AuctionID, &b.UserID, &b.Amount, &b.PrevBidID, &b.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Bid{}, false, nil
	}
	if err != nil {
		return Bid{}, false, fmt.Errorf("look up idempotency key: %w", err)
	}
	return b, true, nil
}

const auctionColumns = `
	SELECT id, item_id, start_at, end_at, starting_price, min_increment, status,
	       current_bid_id, current_leader_id, current_price
	FROM auctions`

func lockAuction(ctx context.Context, tx pgx.Tx, id int64) (Auction, error) {
	return scanAuction(tx.QueryRow(ctx, auctionColumns+` WHERE id = $1 FOR UPDATE`, id))
}

func scanAuction(row pgx.Row) (Auction, error) {
	var a Auction
	var headBid, headUser, headPrice *int64
	err := row.Scan(&a.ID, &a.ItemID, &a.StartAt, &a.EndAt, &a.StartingPrice, &a.MinIncrement, &a.Status,
		&headBid, &headUser, &headPrice)
	if errors.Is(err, pgx.ErrNoRows) {
		return Auction{}, ErrAuctionNotFound
	}
	if err != nil {
		return Auction{}, fmt.Errorf("read auction: %w", err)
	}
	// The schema guarantees the three head columns are all set or all NULL.
	if headBid != nil {
		a.Head = &Head{BidID: *headBid, UserID: *headUser, Price: *headPrice}
	}
	return a, nil
}

// mapDBError translates the guard trigger's SQLSTATEs into domain errors.
// Normally validateBid rejects a bid before the trigger sees it; reaching
// the trigger means the Go rules and the SQL rules disagree, and the SQL
// side won, which is the point of having it.
func mapDBError(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	switch pgErr.Code {
	case "AE001":
		return fmt.Errorf("%w (database guard)", ErrAuctionNotOpen)
	case "AE002":
		return fmt.Errorf("%w (database guard)", ErrAuctionNotStarted)
	case "AE003":
		return fmt.Errorf("%w (database guard)", ErrAuctionEnded)
	case "AE005":
		return fmt.Errorf("%w (database guard)", ErrSelfOutbid)
	case "AE006":
		return fmt.Errorf("%w (database guard)", ErrBidTooLow)
	}
	// AE004 (stale head), AE007 (bid without event) and anything else are
	// bugs or infrastructure failures, not user errors: pass them through.
	return err
}

func isConstraintViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == constraint
}
