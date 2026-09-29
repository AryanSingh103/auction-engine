// Package closer closes auctions once they end (docs/decisions/027). Any
// number of closers can run; one at a time leads, chosen by a session-level
// Postgres advisory lock.
//
// Leadership is an optimization, not a correctness mechanism. Closing goes
// through auction.Service.CloseAuction, which locks the auction row,
// re-reads end_at under the lock and is a no-op on a closed auction. So two
// closers that both believe they lead (a split brain) only duplicate work:
// they can never close an auction early or twice.
package closer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AryanSingh103/auction-engine/internal/auction"
)

// LockKey is the closer's session-level advisory lock: "closer" in ASCII,
// in the same namespace as outbox.LockKey.
const LockKey int64 = 0x636c6f736572

// dbTimeout bounds each leadership query (try-lock, liveness ping) so a
// black-holed connection is noticed instead of hanging the loop.
const dbTimeout = 5 * time.Second

// Results of one close attempt, for metrics.
const (
	ResultClosed   = "closed"   // this call closed it
	ResultAlready  = "already"  // someone else closed it first
	ResultExtended = "extended" // a bid moved end_at after the scan (anti-snipe)
	ResultError    = "error"
)

// Observer receives leadership changes and close results, for metrics.
type Observer interface {
	Leader(bool)
	Closed(result string, lag time.Duration)
}

type nopObserver struct{}

func (nopObserver) Leader(bool)                  {}
func (nopObserver) Closed(string, time.Duration) {}

// Closer is one closer process.
type Closer struct {
	pool     *pgxpool.Pool
	svc      *auction.Service
	interval time.Duration
	batch    int
	obs      Observer
	log      *slog.Logger
}

// New returns a closer. obs may be nil.
func New(pool *pgxpool.Pool, svc *auction.Service, interval time.Duration, batch int, obs Observer, log *slog.Logger) *Closer {
	if obs == nil {
		obs = nopObserver{}
	}
	return &Closer{pool: pool, svc: svc, interval: interval, batch: batch, obs: obs, log: log}
}

// Run campaigns for leadership and, while leading, closes ended auctions,
// until ctx is cancelled.
func (c *Closer) Run(ctx context.Context) {
	for ctx.Err() == nil {
		if err := c.campaign(ctx); err != nil && ctx.Err() == nil {
			c.log.Warn("closer lost its database session; campaigning again", slog.Any("error", err))
		}
		if !sleep(ctx, c.interval) {
			return
		}
	}
}

// campaign holds one dedicated connection: it waits on it until it gets
// the leader lock, then leads until the connection fails or ctx ends.
//
// The connection is taken out of the pool (Hijack) because a session
// advisory lock belongs to the connection: returned to the pool, it would
// stay locked under some unrelated query. Closing the connection is what
// releases the lock, on every exit path, including a crash (Postgres ends
// the session when the TCP connection dies).
func (c *Closer) campaign(ctx context.Context) error {
	pc, err := c.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection: %w", err)
	}
	conn := pc.Hijack()
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dbTimeout)
		defer cancel()
		_ = conn.Close(closeCtx)
	}()

	for {
		held, err := tryLock(ctx, conn)
		if err != nil {
			return err
		}
		if held {
			break
		}
		if !sleep(ctx, c.interval) {
			return nil
		}
	}

	c.log.Info("closer is the leader")
	c.obs.Leader(true)
	defer c.obs.Leader(false)
	for {
		// A live session still holds its lock: session advisory locks end
		// only by unlock or session end. If Postgres ended the session (or
		// the network dropped), another closer may lead by now, so stop.
		if err := ping(ctx, conn); err != nil {
			return fmt.Errorf("leader session: %w", err)
		}
		// A full, clean batch means more may be due: scan again at once.
		// After any failure, wait instead, so an auction that keeps failing
		// cannot turn this into a hot loop.
		for n, failed := c.closeDue(ctx); n == c.batch && !failed; n, failed = c.closeDue(ctx) {
		}
		if !sleep(ctx, c.interval) {
			return nil
		}
	}
}

func tryLock(ctx context.Context, conn *pgx.Conn) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	var held bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, LockKey).Scan(&held); err != nil {
		return false, fmt.Errorf("try leader lock: %w", err)
	}
	return held, nil
}

func ping(ctx context.Context, conn *pgx.Conn) error {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	return conn.Ping(ctx)
}

// closeDue closes up to one batch of ended auctions. It returns how many
// it found and whether anything failed. The scan is only a hint: CloseAuction re-checks everything
// under the row lock, so a bid that extends end_at after the scan wins.
func (c *Closer) closeDue(ctx context.Context) (found int, failed bool) {
	rows, err := c.pool.Query(ctx, `
		SELECT id, end_at FROM auctions
		WHERE status = 'open' AND end_at <= clock_timestamp()
		ORDER BY end_at
		LIMIT $1`, c.batch)
	if err != nil {
		c.log.Error("scan for ended auctions", slog.Any("error", err))
		return 0, true
	}
	type due struct {
		id    int64
		endAt time.Time
	}
	ended, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (due, error) {
		var d due
		return d, r.Scan(&d.id, &d.endAt)
	})
	if err != nil {
		c.log.Error("scan for ended auctions", slog.Any("error", err))
		return 0, true
	}

	for _, d := range ended {
		if ctx.Err() != nil {
			return 0, true
		}
		closed, err := c.svc.CloseAuction(ctx, d.id)
		switch {
		case err == nil && closed:
			// Lag on this process's clock against the database's end_at;
			// clock skew between the two shows up here.
			c.obs.Closed(ResultClosed, time.Since(d.endAt))
		case err == nil:
			c.obs.Closed(ResultAlready, 0)
		case errors.Is(err, auction.ErrAuctionNotEnded):
			c.obs.Closed(ResultExtended, 0)
		default:
			failed = true
			c.obs.Closed(ResultError, 0)
			if ctx.Err() == nil {
				c.log.Error("close auction", slog.Int64("auction_id", d.id), slog.Any("error", err))
			}
		}
	}
	return len(ended), failed
}

// sleep waits d or until ctx ends, reporting whether it waited the full d.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
