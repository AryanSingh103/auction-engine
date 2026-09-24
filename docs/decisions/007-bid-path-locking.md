# 007. Bid path: pessimistic row lock under READ COMMITTED

## Context
Every bid on one auction contends on one row. The brief mandates `SELECT … FOR UPDATE`.

## Decision
One READ COMMITTED transaction: check the user; lock the auction row; look up the idempotency key; read `clock_timestamp()` in a **separate statement**; validate; insert the bid, head and outbox event; commit.

**Measured (R1):** in a `FOR UPDATE` select, `clock_timestamp()` in the target list was evaluated **before** the lock wait ended (660ms stale) when the holder didn't modify the row. Reading the clock afterwards is required, and a test fails if that regresses.

## Alternatives considered
- **Optimistic locking** (a version column, compare-and-set, retry): no waiting, but on a single hot row most attempts fail and retry. M2 benchmarks it.
- **SERIALIZABLE:** correct without explicit locks, but conflicts abort with 40001 and need retry loops.

## Consequences
Bids on one auction serialize: 1000 concurrent bids took about 1.1s locally (`-race`, not a benchmark). Mutation tests showed the guard trigger and the fork-proof constraint each keep the data valid without the app lock, but the correct winner can then be rejected. The app lock is the primary mechanism.
