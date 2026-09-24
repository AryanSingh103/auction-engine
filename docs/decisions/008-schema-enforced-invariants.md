# 008. Invariants enforced by the schema, not only by Go

## Context
The brief asks for invariants enforced by the database, not hopeful app logic. A bug or bypass in the Go bid path would otherwise corrupt state silently.

## Decision
The database is the backstop:
- **Fork-proof chain:** each bid names its predecessor (`prev_bid_id`), and `UNIQUE NULLS NOT DISTINCT (auction_id, prev_bid_id)` means no two bids extend the same head. Lost updates are impossible even without locks (tested with the guard disabled).
- **Guard trigger:** it locks the auction row, reads the clock after the lock, and rejects bids that are out of the window, on a closed auction, too low, stale or self-outbids (`AE001`–`AE006`).
- **Invariant 6:** a FK from outbox to bid, plus a deferred constraint trigger that fails COMMIT (`AE007`) when a bid has no event.
- **Composite FKs** keep predecessor, head and event within the same auction.

## Alternatives considered
Go-only validation: simpler, but one missed lock breaks invariants silently.

## Consequences
Rules live in Go and SQL; tests pin both. The re-lock costs something, which M2 measures. Bids are append-only by convention until M6.
