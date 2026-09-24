# 014. Optimistic bid strategy, kept for benchmarking

## Context
The brief asks for a measured pessimistic vs optimistic comparison. A throwaway branch would make it unrepeatable.

## Decision
`BID_LOCKING=optimistic` selects a second bid path:
- read the auction **without** a lock, then validate and insert the successor of the head it saw
- a moved head is caught by the guard's stale-head check (`AE004`) or the no-fork constraint, and the transaction retries (up to 10 attempts with full-jitter backoff, then 503 `contention`)
- a validation failure re-checks the idempotency key first, so a racing copy of the request replays instead of being rejected

This is **optimistic read, lock at write**, not lock-free: the guard still locks during the INSERT. Removing the guard would weaken invariants 3 and 4. Pessimistic stays the default; both run the full correctness suite.

## Consequences
- **Invariant 4 rests solely on the guard trigger** under optimistic: the Go clock read happens before the lock (tested).
- Stale snapshots can produce a spurious `self_outbid` (retryable). "Too low" stays correct because prices only rise.
- Doomed bids skip the lock queue: why optimistic was ~3× faster in a 99%-rejection test. The benchmark must separate this.
