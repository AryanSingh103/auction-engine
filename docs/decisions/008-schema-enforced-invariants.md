# 008. Invariants enforced by the schema, not only by Go

## Context
The brief asks for invariants enforced by the database, not hopeful app logic. A bug or bypass in the Go bid path would otherwise corrupt state silently.

## Decision
The database is the backstop:
- **Fork-proof chain:** each bid names its predecessor (`prev_bid_id`), and `UNIQUE NULLS NOT DISTINCT (auction_id, prev_bid_id)` means no two bids extend the same head. A lost update is impossible even with no locks at all (tested with the guard disabled).
- **Guard trigger:** it locks the auction row and rejects bids that are outside the window, on a closed auction, too low, stale, or self-outbids (SQLSTATE `AE001`–`AE006`). The clock is read only after the lock is held.
- **Invariant 6:** a FK from outbox to bid, plus a deferred constraint trigger that fails COMMIT (`AE007`) when a bid has no event.
- **Composite FKs** keep predecessor, head and event within the same auction.

## Alternatives considered
Go-only validation: simpler, but one missed lock breaks invariants silently.

## Consequences
The rules live in two places (Go and SQL), and tests pin both. Each insert re-locks a row the bid path already holds, which M2 will measure. Bids are append-only by convention; privileges come in M6.
