# 008. Invariants enforced by the schema, not only by Go

## Context
The brief asks for invariants enforced by the database, not hopeful app logic.

## Decision
- **Fork-proof chain:** each bid names its predecessor (`prev_bid_id`), and `UNIQUE NULLS NOT DISTINCT (auction_id, prev_bid_id)` stops two bids extending one head. Lost updates are impossible even without locks.
- **Guard trigger:** it locks the auction, reads the clock after the lock, sets `created_at`, and rejects bids that are out of the window, on a closed auction, too low, stale, or self-outbids (`AE001`–`AE006`).
- **Invariant 6:** outbox→bid FK, plus a deferred trigger that fails COMMIT for a bid with no event (`AE007`).
- **Head integrity:** a composite FK ties the head's bid, leader and price to one real bid. The head only advances one link (`AE011`), and status only goes open→closed (`AE010`).
- **Append-only:** bids are immutable (`AE008`). Outbox rows can only be marked published once (`AE009`).

## Alternatives considered
Go-only validation: simpler, but one missed lock breaks invariants silently.

## Consequences
Rules live in Go and SQL; tests pin both. The re-lock has a cost (M2 measures it). `AE008`–`AE011` were added after the M1 review found those fields mutable.
