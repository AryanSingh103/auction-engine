# 016. Keep pessimistic locking as the default, after measuring

## Context
M2 benchmarked both bid strategies (see `docs/benchmarks.md`, 2026-09-24): 3 reps × 7 configurations, every run verified.

## Decision
**Pessimistic stays the default.**
- At low contention the two are within noise.
- At moderate contention (10 bidders on one auction) pessimistic has 14% more throughput, 23% more accepted bids/s, and half the p99, because optimistic wasted about 3 attempts per accepted bid.
- Pessimistic enforces invariant 4 in Go. Under optimistic, only the guard trigger enforces it.

**Optimistic wins only at extreme contention** (200 bidders, 98% doomed bids): +64% throughput, and the reason isn't optimism. It rejects doomed bids *before* any lock, which is safe because prices only rise. Pessimistic makes every doomed bid queue for the lock.

## Alternatives considered
- **Switch to optimistic:** better only in one regime, worse at moderate contention, and weaker layering for invariant 4.
- **Hybrid** (an unlocked "too low" pre-check, then the pessimistic path): likely the best of both, but unmeasured. Tracked as R15.

## Consequences
The optimistic path stays, behind `BID_LOCKING`, for re-running the comparison (M7 graphs). At 200 bidders the 20-connection pool is also a queue.
