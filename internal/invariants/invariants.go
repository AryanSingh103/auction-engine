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
	// Needs, if set, is a table the check reads that may not exist where
	// the check runs (paysim.charges exists only where the payment
	// simulator has run). Without it the check is skipped, and Run
	// reports that it was.
	Needs string
	// Drained marks an eventual property: it holds once the relay and the
	// settlers have caught up, not at every instant (mid-flight, a closed
	// auction may not be invoiced yet, and a lost response leaves a charge
	// on a pending invoice). Run includes it only in DrainedMode.
	Drained bool
}

// Mode selects which checks Run executes.
type Mode int

const (
	// SafetyMode runs the checks that must hold at every instant.
	SafetyMode Mode = iota
	// DrainedMode also runs the eventual ones. Use it only when nothing is
	// in flight: no unpublished outbox events and no pending settlement.
	DrainedMode
)

// Checks lists every invariant that can be verified at this milestone.
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
	{
		// Closing: settlement hears of every close exactly once.
		Name: "closed auctions have one close event",
		SQL: `
			SELECT format('auction %s (%s) has %s auction_closed events', a.id, a.status, count(o.id))
			FROM auctions a
			LEFT JOIN outbox o ON o.auction_id = a.id AND o.event_type = 'auction_closed'
			GROUP BY a.id, a.status
			HAVING count(o.id) <> CASE WHEN a.status = 'closed' THEN 1 ELSE 0 END`,
	},
	{
		// Invariant 5, our side: one invoice per closed auction with a
		// winner, and settled. "failed" counts as settled: the provider
		// definitively declined, so there is correctly no charge (R5).
		Name: "closed auctions with a winner have one settled invoice",
		SQL: `
			SELECT format('auction %s: %s invoice(s), statuses %s', a.id, count(i.id), coalesce(string_agg(i.status, ','), '-'))
			FROM auctions a LEFT JOIN invoices i ON i.auction_id = a.id
			WHERE a.status = 'closed' AND a.current_bid_id IS NOT NULL
			GROUP BY a.id
			HAVING count(i.id) <> 1 OR bool_or(i.status = 'pending')`,
		Drained: true,
	},
	{
		Name: "invoices match their auction's result",
		SQL: `
			SELECT format('invoice %s: bid %s, user %s, amount %s; auction %s is %s with head %s, user %s, price %s',
			              i.id, i.bid_id, i.winner_id, i.amount, a.id, a.status, a.current_bid_id, a.current_leader_id, a.current_price)
			FROM invoices i JOIN auctions a ON a.id = i.auction_id
			WHERE a.status <> 'closed'
			   OR a.current_bid_id IS DISTINCT FROM i.bid_id
			   OR a.current_leader_id IS DISTINCT FROM i.winner_id
			   OR a.current_price IS DISTINCT FROM i.amount`,
	},
	{
		// Invariant 5, provider side: a paid invoice records the one charge
		// the provider made under its key, for its amount and winner.
		Name: "paid invoices have exactly their charge",
		SQL: `
			SELECT format('invoice %s paid with %s for %s to user %s; provider has %s',
			              i.id, i.payment_id, i.amount, i.winner_id,
			              -- format() renders NULLs as empty strings, so coalesce would not fire
			              CASE WHEN c.id IS NULL THEN 'no charge' ELSE format('%s for %s to user %s', c.id, c.amount, c.customer_id) END)
			FROM invoices i LEFT JOIN paysim.charges c ON c.idempotency_key = 'invoice-' || i.id
			WHERE i.status = 'paid'
			  AND (c.id IS DISTINCT FROM i.payment_id OR c.amount <> i.amount OR c.customer_id <> i.winner_id)`,
		Needs: "paysim.charges",
	},
	{
		// The other direction, at every instant: a charge on a failed or
		// missing invoice is money taken that our books will never show
		// (found by the M4 review: this was drained-only before).
		Name: "no charge on a failed or missing invoice",
		SQL: `
			SELECT format('charge %s (key %s) but the invoice is %s', c.id, c.idempotency_key, coalesce(i.status, 'missing'))
			FROM paysim.charges c
			LEFT JOIN invoices i ON c.idempotency_key = 'invoice-' || i.id
			WHERE c.idempotency_key LIKE 'invoice-%' AND (i.id IS NULL OR i.status = 'failed')`,
		Needs: "paysim.charges",
	},
	{
		// Once drained, a charge on a pending invoice (a lost response not
		// yet replayed) must have been recorded as paid.
		Name: "no charge left on a pending invoice",
		SQL: `
			SELECT format('charge %s (key %s) but the invoice is still pending', c.id, c.idempotency_key)
			FROM paysim.charges c
			JOIN invoices i ON c.idempotency_key = 'invoice-' || i.id
			WHERE i.status = 'pending'`,
		Needs:   "paysim.charges",
		Drained: true,
	},
}

// Violation is one broken invariant instance.
type Violation struct {
	Check  string
	Detail string
}

func (v Violation) String() string { return v.Check + ": " + v.Detail }

// Run executes the checks for mode and returns all violations found, and
// the names of checks skipped because a table they need does not exist.
// An error means a check could not run, not that an invariant failed.
func Run(ctx context.Context, q Querier, mode Mode) (violations []Violation, skipped []string, err error) {
	for _, c := range Checks {
		if c.Drained && mode != DrainedMode {
			continue
		}
		if c.Needs != "" {
			exists, err := tableExists(ctx, q, c.Needs)
			if err != nil {
				return nil, nil, fmt.Errorf("check %q: %w", c.Name, err)
			}
			if !exists {
				skipped = append(skipped, c.Name)
				continue
			}
		}
		rows, err := q.Query(ctx, c.SQL)
		if err != nil {
			return nil, nil, fmt.Errorf("check %q: %w", c.Name, err)
		}
		details, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return nil, nil, fmt.Errorf("check %q: %w", c.Name, err)
		}
		for _, d := range details {
			violations = append(violations, Violation{Check: c.Name, Detail: d})
		}
	}
	return violations, skipped, nil
}

func tableExists(ctx context.Context, q Querier, table string) (bool, error) {
	rows, err := q.Query(ctx, `SELECT to_regclass($1) IS NOT NULL`, table)
	if err != nil {
		return false, err
	}
	return pgx.CollectExactlyOneRow(rows, pgx.RowTo[bool])
}
