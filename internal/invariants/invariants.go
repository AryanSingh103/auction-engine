// Package invariants checks the project's invariants against the data
// actually stored in Postgres, after the fact. The schema prevents most
// violations up front; this is the independent audit that runs after tests
// and load runs (and, from milestone 7, after fault injection), so that a
// hole in the up-front enforcement shows up as a named violation.
//
// Each check is one read-only query whose rows ARE the violations: an empty
// result means the invariant holds.
package invariants

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Querier is satisfied by *pgxpool.Pool, *pgx.Conn and pgx.Tx.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Check is one named invariant query.
type Check struct {
	Name string
	// SQL returns one row per violation, as a single text column.
	SQL string
}

// Checks lists every invariant that can be verified at this milestone.
// Invariant 1 (exactly one winner) and 5 (exactly one charge) arrive with
// closing (M5) and settlement (M4).
var Checks = []Check{
	{
		// Invariant 3, first link: the first accepted bid meets the
		// starting price.
		Name: "first bid meets starting price",
		SQL: `
			SELECT format('bid %s on auction %s: %s < starting price %s', b.id, a.id, b.amount, a.starting_price)
			FROM bids b JOIN auctions a ON a.id = b.auction_id
			WHERE b.prev_bid_id IS NULL AND b.amount < a.starting_price`,
	},
	{
		// Invariant 3: every accepted bid beats the one it outbid by at
		// least min_increment.
		Name: "each bid beats its predecessor by the increment",
		SQL: `
			SELECT format('bid %s on auction %s: %s < previous %s + increment %s', b.id, a.id, b.amount, p.amount, a.min_increment)
			FROM bids b
			JOIN bids p ON p.id = b.prev_bid_id
			JOIN auctions a ON a.id = b.auction_id
			WHERE b.amount < p.amount + a.min_increment`,
	},
	{
		// The bids of an auction form one unbroken chain: exactly one has no
		// predecessor, and every bid is reachable from it. Counting chain
		// members from the root and comparing with all bids catches both
		// forks and orphans.
		Name: "bids form a single chain",
		SQL: `
			WITH RECURSIVE chain AS (
				SELECT id, auction_id FROM bids WHERE prev_bid_id IS NULL
				UNION ALL
				SELECT b.id, b.auction_id FROM bids b JOIN chain c ON b.prev_bid_id = c.id
			)
			SELECT format('auction %s: %s bids but %s reachable in the chain', t.auction_id, t.total, coalesce(c.n, 0))
			FROM (SELECT auction_id, count(*) AS total FROM bids GROUP BY auction_id) t
			LEFT JOIN (SELECT auction_id, count(*) AS n FROM chain GROUP BY auction_id) c USING (auction_id)
			WHERE c.n IS DISTINCT FROM t.total`,
	},
	{
		// Invariant 2: the auction's head (its current leader) is the
		// highest accepted bid, and the head columns agree with that bid.
		Name: "head is the highest bid",
		SQL: `
			SELECT format('auction %s: head bid %s (price %s, leader %s) but highest bid is %s by user %s (%s)',
			              a.id, a.current_bid_id, a.current_price, a.current_leader_id, top.id, top.user_id, top.amount)
			FROM auctions a
			JOIN LATERAL (
				SELECT id, user_id, amount FROM bids WHERE auction_id = a.id ORDER BY amount DESC, id LIMIT 1
			) top ON true
			WHERE a.current_bid_id IS DISTINCT FROM top.id
			   OR a.current_price IS DISTINCT FROM top.amount
			   OR a.current_leader_id IS DISTINCT FROM top.user_id`,
	},
	{
		Name: "auctions without bids have no head",
		SQL: `
			SELECT format('auction %s has head bid %s but no bids', a.id, a.current_bid_id)
			FROM auctions a
			WHERE a.current_bid_id IS NOT NULL
			  AND NOT EXISTS (SELECT 1 FROM bids b WHERE b.auction_id = a.id)`,
	},
	{
		// Invariant 4: no bid was accepted outside the auction window.
		Name: "bids accepted inside the auction window",
		SQL: `
			SELECT format('bid %s on auction %s created at %s, outside [%s, %s)', b.id, a.id, b.created_at, a.start_at, a.end_at)
			FROM bids b JOIN auctions a ON a.id = b.auction_id
			WHERE b.created_at < a.start_at OR b.created_at >= a.end_at`,
	},
	{
		Name: "nobody outbids themselves",
		SQL: `
			SELECT format('bid %s by user %s outbid their own bid %s', b.id, b.user_id, p.id)
			FROM bids b JOIN bids p ON p.id = b.prev_bid_id
			WHERE b.user_id = p.user_id`,
	},
	{
		// Invariant 6, both directions.
		Name: "every bid has exactly one outbox event",
		SQL: `
			SELECT format('bid %s has %s bid_placed events', b.id, count(o.id))
			FROM bids b LEFT JOIN outbox o ON o.bid_id = b.id AND o.event_type = 'bid_placed'
			GROUP BY b.id HAVING count(o.id) <> 1`,
	},
	{
		Name: "every bid event has its bid",
		SQL: `
			SELECT format('outbox event %s references missing bid %s', o.id, o.bid_id)
			FROM outbox o LEFT JOIN bids b ON b.id = o.bid_id
			WHERE o.event_type = 'bid_placed' AND b.id IS NULL`,
	},
}

// Violation is one broken invariant instance.
type Violation struct {
	Check  string
	Detail string
}

func (v Violation) String() string { return v.Check + ": " + v.Detail }

// Run executes every check and returns all violations found. An error means
// a check could not run, not that an invariant failed.
func Run(ctx context.Context, q Querier) ([]Violation, error) {
	var out []Violation
	for _, c := range Checks {
		rows, err := q.Query(ctx, c.SQL)
		if err != nil {
			return nil, fmt.Errorf("check %q: %w", c.Name, err)
		}
		details, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return nil, fmt.Errorf("check %q: %w", c.Name, err)
		}
		for _, d := range details {
			out = append(out, Violation{Check: c.Name, Detail: d})
		}
	}
	return out, nil
}
