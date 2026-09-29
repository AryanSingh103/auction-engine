# 027. Closer worker and advisory-lock leader election

## Context
Ended auctions must be closed to start settlement. Several closer copies run for availability, and scanning in all of them at once wastes work.

## Decision
- Leadership comes from a **session-level** `pg_try_advisory_lock` on a dedicated connection taken out of the pool (Hijack). Only the leader scans `status = 'open' AND end_at <= clock_timestamp()` (partial index `auctions_open_by_end`) and calls `CloseAuction`. Standbys retry the lock every `CLOSER_INTERVAL`.
- Before each scan the leader pings its session. The lock lives exactly as long as the session, so a failed ping means it may have lost the lead, and it re-campaigns. Closing the connection, or a crash, releases the lock.
- **Correctness never depends on leadership.** `CloseAuction` locks the row, re-reads `end_at` and is idempotent, so a split brain only duplicates work.

## Alternatives
- **Transaction-level lock per scan** (the relay's way): no failover state, but no single leader either, and the lock is re-contended every tick.
- **Lease row with expiry:** it works across databases but needs clock-based fencing. The advisory lock is tied to a real session.
- **No leader** (row locks alone are safe): correct, but N copies scan and contend.

## Consequences
An auction closes about one interval late. That costs nothing, since bids are refused from `end_at` on. The closer has no Redis, so a cached "open" can last up to `AUCTION_CACHE_TTL`.
