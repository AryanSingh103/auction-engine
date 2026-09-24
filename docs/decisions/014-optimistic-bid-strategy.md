# 014. Optimistic bid strategy, kept for benchmarking

## Context
The brief asks for a measured pessimistic vs optimistic comparison, which should stay repeatable.

## Decision
`BID_LOCKING=optimistic` selects a second bid path:
- read the auction **without** a lock, then validate and insert the successor of the head it saw
- a moved head is caught by the guard's stale-head check (`AE004`) or the no-fork constraint, and the transaction retries (10 attempts, full-jitter backoff, then 503 `contention`)
- a validation failure re-checks the idempotency key first, so a racing copy of the request replays instead of being rejected

It's **optimistic read, lock at write**: the guard still locks during the INSERT (removing it would weaken invariants 3 and 4). Both strategies run the full correctness suite.

## Consequences
- **Invariant 4 rests solely on the guard trigger** under optimistic: the Go clock read happens before the lock (tested).
- Stale snapshots can cause a spurious `self_outbid` 409 (the client must re-read). Untested: the load generator can't trigger it. "Too low" stays correct, since prices only rise.
- Doomed bids skip the lock queue, a suspected cause of it looking ~3× faster in one M1 `-race` test run (not a benchmark).
