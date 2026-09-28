// Package outbox publishes the transactional outbox to Kafka
// (docs/decisions/022). Events are written to the outbox table in the same
// transaction as the change they describe (invariant 6); the relay here
// delivers them to Kafka at least once, in outbox order.
package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Event is one outbox row as published: the JSON value of the Kafka record,
// whose key is the auction id. Consumers deduplicate on ID, because
// delivery is at least once.
type Event struct {
	ID        int64           `json:"id"`
	Type      string          `json:"type"`
	AuctionID int64           `json:"auction_id"`
	CreatedAt time.Time       `json:"created_at"`
	Payload   json.RawMessage `json:"payload"`
}

// Publisher delivers events to the broker, in order. It returns nil only
// once every event is durably acknowledged. On error, some events may have
// been delivered anyway; the relay sends them again.
type Publisher interface {
	Publish(ctx context.Context, events []Event) error
}

// LockKey is the relay's transaction-level advisory lock. Advisory lock
// keys share one namespace per database; this is "outbox" in ASCII so it
// cannot collide with small integers another feature might pick.
const LockKey int64 = 0x6f7574626f78

// Observer receives the outcome of every batch, for metrics.
type Observer interface {
	// Batch reports one PublishBatch: result is "published" (n events,
	// possibly 0), "standby" (another relay held the lock) or "failed".
	Batch(result string, n int, d time.Duration)
}

type nopObserver struct{}

func (nopObserver) Batch(string, int, time.Duration) {}

// Relay moves events from the outbox table to a Publisher.
type Relay struct {
	pool           *pgxpool.Pool
	pub            Publisher
	batchSize      int
	publishTimeout time.Duration
	obs            Observer
}

// NewRelay returns a relay that publishes up to batchSize events per
// transaction, giving each Publish call publishTimeout. obs may be nil.
func NewRelay(pool *pgxpool.Pool, pub Publisher, batchSize int, publishTimeout time.Duration, obs Observer) *Relay {
	if obs == nil {
		obs = nopObserver{}
	}
	return &Relay{pool: pool, pub: pub, batchSize: batchSize, publishTimeout: publishTimeout, obs: obs}
}

// PublishBatch publishes the oldest unpublished events (up to the batch
// size) and marks them published, in one transaction that holds the relay
// lock. It reports how many it published, and whether this relay held the
// lock; if not, another relay is active and nothing was done.
//
// Why a transaction-level lock rather than a session lock held for as long
// as the process lives: a session lock is released when its connection
// dies, even if the process has not noticed, so a second relay could take
// over while the first is still publishing. Here the lock only lives as
// long as the transaction, and a relay whose connection died cannot commit
// its marks, so the worst case is that its events are published twice.
//
// Order: events of one auction are inserted while holding that auction's
// row lock, which is held until commit, so their ids are in commit order.
// Reading committed rows in id order therefore keeps each auction's events
// in order, even though a slow transaction on another auction may commit a
// lower id later (it is simply published in a later batch).
func (r *Relay) PublishBatch(ctx context.Context) (published int, held bool, err error) {
	start := time.Now()
	defer func() {
		switch {
		case err != nil:
			r.obs.Batch("failed", 0, time.Since(start))
		case !held:
			r.obs.Batch("standby", 0, time.Since(start))
		default:
			r.obs.Batch("published", published, time.Since(start))
		}
	}()
	err = pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`, LockKey).Scan(&held); err != nil {
			return fmt.Errorf("take relay lock: %w", err)
		}
		if !held {
			return nil
		}

		rows, err := tx.Query(ctx, `
			SELECT id, event_type, auction_id, created_at, payload
			FROM outbox
			WHERE published_at IS NULL
			ORDER BY id
			LIMIT $1`, r.batchSize)
		if err != nil {
			return fmt.Errorf("read outbox: %w", err)
		}
		events, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Event, error) {
			var e Event
			err := row.Scan(&e.ID, &e.Type, &e.AuctionID, &e.CreatedAt, &e.Payload)
			return e, err
		})
		if err != nil {
			return fmt.Errorf("read outbox: %w", err)
		}
		if len(events) == 0 {
			return nil
		}

		// The transaction stays open while Kafka acknowledges. The pool's
		// idle_in_transaction_session_timeout ends it if Publish hangs past
		// that, so a stuck broker cannot hold the lock forever; the batch is
		// then published again by whichever relay runs next.
		pctx, cancel := context.WithTimeout(ctx, r.publishTimeout)
		defer cancel()
		if err := r.pub.Publish(pctx, events); err != nil {
			return fmt.Errorf("publish %d events: %w", len(events), err)
		}

		ids := make([]int64, len(events))
		for i, e := range events {
			ids[i] = e.ID
		}
		tag, err := tx.Exec(ctx, `UPDATE outbox SET published_at = clock_timestamp() WHERE id = ANY($1)`, ids)
		if err != nil {
			return fmt.Errorf("mark published: %w", err)
		}
		// Under the lock no one else marks these rows; a mismatch means the
		// lock did not do its job, and committing would hide that.
		if tag.RowsAffected() != int64(len(ids)) {
			return fmt.Errorf("marked %d of %d events published", tag.RowsAffected(), len(ids))
		}
		published = len(events)
		return nil
	})
	return published, held, err
}

// Run publishes until ctx is cancelled. After a full batch it goes again
// at once; otherwise, including after an error, it waits interval. An
// event is never skipped: a failed batch is retried from the same row.
//
// With several relays running there is no stable leader: the lock is per
// batch, so whichever relay polls first runs the next one. That is by
// design (a dead relay needs no failover), so it is not logged.
func (r *Relay) Run(ctx context.Context, interval time.Duration, logger *slog.Logger) {
	for {
		n, _, err := r.PublishBatch(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			logger.Error("outbox batch failed; will retry", slog.Any("error", err))
		} else if n == r.batchSize {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}
