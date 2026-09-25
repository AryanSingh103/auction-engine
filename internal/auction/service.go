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
	pool    *pgxpool.Pool
	obs     Observer
	locking Locking
	cache   Cache // optional
}

// NewService returns a Service using pool.
func NewService(pool *pgxpool.Pool, opts ...Option) *Service {
	s := &Service{pool: pool, obs: nopObserver{}, locking: LockingPessimistic}
	for _, o := range opts {
		o(s)
	}
	return s
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
	defer func() { s.obs.BidOutcome(OutcomeOf(err, replayed)) }()

	if s.locking == LockingOptimistic {
		bid, replayed, err = s.placeBidOptimistic(ctx, req)
	} else {
		bid, replayed, err = s.placeBidTx(ctx, req)
	}
	if err == nil && !replayed {
		s.refreshCache(ctx, req.AuctionID)
	}
	if isConstraintViolation(err, "bids_idempotency") {
		// Same-key requests on the SAME auction are serialized by the row
		// lock and resolved by the lookup inside the transaction. This path
		// covers same-key requests on DIFFERENT auctions, which lock
		// different rows: the loser's insert hits the unique constraint,
		// its transaction rolls back, and it answers as a later retry would
		// (here, always ErrIdempotencyConflict, since the auction differs).
		return s.replay(ctx, req)
	}
	return bid, replayed, err
}

func (s *Service) placeBidTx(ctx context.Context, req PlaceBidRequest) (bid Bid, replayed bool, err error) {
	txStart := time.Now()
	defer func() { s.obs.BidTransaction(time.Since(txStart)) }()

	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// 1. The bidder must exist. Checked before the lock so an unknown
		// user never holds up real bidders, and before validation so the
		// caller learns about identity before bid rules.
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE id = $1)`, req.UserID).Scan(&exists); err != nil {
			return fmt.Errorf("check user: %w", err)
		}
		if !exists {
			return ErrUnknownUser
		}

		// 2. Lock the auction row. Every other bid on this auction now waits
		// here until we commit or roll back.
		lockStart := time.Now()
		a, err := lockAuction(ctx, tx, req.AuctionID)
		s.obs.LockWait(time.Since(lockStart))
		if err != nil {
			return err
		}

		// 3. Idempotency: a retry of an accepted bid returns the original.
		// This MUST come after the lock. If it ran before, two copies of the
		// same request could both miss here; the first then commits and
		// becomes the leader, and the second, once it gets the lock, is
		// rejected as "self-outbid" instead of being answered as a replay.
		// After the lock, this statement sees every bid committed by earlier
		// lock holders (READ COMMITTED takes a fresh snapshot per statement).
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
		bid, err = writeBid(ctx, tx, req, a, nil)
		return err
	})
	if err != nil {
		return Bid{}, false, mapDBError(err)
	}
	return bid, replayed, nil
}

// writeBid inserts the bid as the successor of a's head, advances the head
// and writes the bid_placed outbox event. If insertTimed is non-nil it is
// given the INSERT's duration: in the optimistic strategy that statement is
// where the guard trigger takes the auction row lock.
func writeBid(ctx context.Context, tx pgx.Tx, req PlaceBidRequest, a Auction, insertTimed func(time.Duration)) (Bid, error) {
	var prev *int64
	if a.Head != nil {
		prev = &a.Head.BidID
	}
	bid := Bid{AuctionID: req.AuctionID, UserID: req.UserID, Amount: req.Amount, PrevBidID: prev}
	insertStart := time.Now()
	err := tx.QueryRow(ctx, `
		INSERT INTO bids (auction_id, user_id, amount, prev_bid_id, idempotency_key)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, created_at`,
		req.AuctionID, req.UserID, req.Amount, prev, req.IdempotencyKey,
	).Scan(&bid.ID, &bid.CreatedAt)
	if insertTimed != nil {
		insertTimed(time.Since(insertStart))
	}
	if err != nil {
		return Bid{}, fmt.Errorf("insert bid: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		UPDATE auctions
		SET current_price = $2, current_leader_id = $3, current_bid_id = $4
		WHERE id = $1`,
		req.AuctionID, req.Amount, req.UserID, bid.ID); err != nil {
		return Bid{}, fmt.Errorf("advance auction head: %w", err)
	}

	payload, err := json.Marshal(bid)
	if err != nil {
		return Bid{}, fmt.Errorf("encode bid_placed event: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO outbox (auction_id, event_type, payload, bid_id)
		VALUES ($1, 'bid_placed', $2, $3)`,
		req.AuctionID, payload, bid.ID); err != nil {
		return Bid{}, fmt.Errorf("insert outbox event: %w", err)
	}
	return bid, nil
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

// GetAuction returns an auction's current state for display, from the cache
// when possible. Cache errors are not errors here: it falls back to
// Postgres (docs/decisions/017).
func (s *Service) GetAuction(ctx context.Context, id int64) (Auction, error) {
	if s.cache != nil {
		if a, ok, err := s.cache.Get(ctx, id); err == nil && ok {
			return a, nil
		}
	}
	a, err := s.readAuction(ctx, id)
	if err != nil {
		return Auction{}, err
	}
	if s.cache != nil {
		_ = s.cache.Put(ctx, a) // a failed or stale put only costs a later miss
	}
	return a, nil
}

// refreshCache writes the auction's post-commit state to the cache so
// readers see the new head immediately rather than after the TTL. It reads
// Postgres rather than reusing the transaction's view, so it can never
// cache a state that was not committed; if a later bid already refreshed
// the cache, the version guard keeps the newer state.
func (s *Service) refreshCache(ctx context.Context, id int64) {
	if s.cache == nil {
		return
	}
	if a, err := s.readAuction(ctx, id); err == nil {
		_ = s.cache.Put(ctx, a)
	}
}

func (s *Service) readAuction(ctx context.Context, id int64) (Auction, error) {
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
		return fmt.Errorf("%w: %w", ErrRejectedByDatabaseGuard, ErrAuctionNotOpen)
	case "AE002":
		return fmt.Errorf("%w: %w", ErrRejectedByDatabaseGuard, ErrAuctionNotStarted)
	case "AE003":
		return fmt.Errorf("%w: %w", ErrRejectedByDatabaseGuard, ErrAuctionEnded)
	case "AE005":
		return fmt.Errorf("%w: %w", ErrRejectedByDatabaseGuard, ErrSelfOutbid)
	case "AE006":
		return fmt.Errorf("%w: %w", ErrRejectedByDatabaseGuard, ErrBidTooLow)
	}
	// AE004 (stale head), AE007 (bid without event) and anything else are
	// bugs or infrastructure failures, not user errors: pass them through.
	return err
}

func isConstraintViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == constraint
}
