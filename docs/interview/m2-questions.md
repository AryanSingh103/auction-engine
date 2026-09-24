# Milestone 2 interview questions

Generated from the M2 diff (`git diff 5e7451a..HEAD`) by the interview-questions subagent. Answer in your own words, whenever you like (below each question, or in chat); Claude then grades the answers and points out where they are wrong or vague.

## 1. Reading lock-wait metrics
*Files: `internal/metrics/bids.go` (with `latencyBuckets` in `metrics.go`, the pool collector in `pool.go`)*

`bid_lock_wait_seconds` times `SELECT ... FOR UPDATE` under pessimistic, but times the bid `INSERT` (where the guard trigger takes the lock) under optimistic. The benchmark table puts both in one "lock wait p99" column, backed by buckets about 1.9× apart.
- An on-call engineer sees that p99 jump from 7.0 to 13.6 ms after a deploy that switched `BID_LOCKING`. What can they actually conclude from `histogram_quantile` over these buckets? What can't they conclude?
- Which other exported series (`db_pool_empty_acquires_total`, `bid_transaction_seconds`, `bids_total{outcome}`, the HTTP histogram by route pattern) would they combine to tell lock contention apart from pool exhaustion?
- How would you change the metric so the two strategies are comparable?

## 2. Correctness of the optimistic path
*File: `internal/auction/optimistic.go` (placeBidOptimisticTx, isOptimisticConflict)*

The optimistic path reads the auction and `clock_timestamp()` with no lock, validates in Go, then inserts.
- For each failure mode, walk through what stops a wrong answer:
  - (a) the auction closes between the validation and the INSERT
  - (b) a second copy of the same idempotency key commits between the first key lookup and the validation
- Why does the key re-check happen only when validation fails?
- Why is it acceptable that invariant 4 now rests only on the guard trigger?
- What change to the schema or trigger (for example, M5's anti-snipe moving `end_at`) would silently break this strategy's correctness while the pessimistic path stayed correct?

## 3. What the load generator's checks can't catch
*File: `internal/loadgen/loadgen.go` (runBids, placeBid, checkServerSawRequests, checkDatabase)*

The checks exist so a run that "measures nothing" fails, and a stub returning 201 without storing anything proves they work.
- What classes of broken measurement would still pass all five checks? Consider:
  - the closed-loop design, and coordinated omission when the API stalls
  - a leading worker switching to GET polling, which changes the offered bid load between strategies
  - 503/500/transport errors retried with the same key until `resolveGrace`
- Could any of these bias the pessimistic vs optimistic comparison in one direction, rather than just adding noise?

## 4. How defensible are the numbers?
*Files: `docs/benchmarks.md` (Method, Results, Caveats), `scripts/bench.sh`*

The methodology: a fresh database per run, 3 interleaved reps, the median with min–max, a `/healthz` control at about 68k req/s, and the load generator, API and Postgres all on the same 4 shared vCPUs. The spread runs vary by ±30% between reps.
- How confident are you that these are real effects and not artifacts?
  - hot-w10: pessimistic +14%
  - hot-w200: optimistic +64%, with a range of 8289–10822
- What does the healthz control prove about a CPU-bound bid path whose load generator steals the same cores, and what doesn't it prove?
- What would you change first to make these numbers defensible to a skeptical reviewer?

## 5. Testing the explanation, not just fitting it
*Files: `docs/decisions/016-keep-pessimistic-locking.md`, with "What the numbers say" in `docs/benchmarks.md` and R15 in `docs/open-questions.md`*

The conclusion: optimistic wins at 200 bidders not because it's optimistic, but because 98% of bids are doomed and get rejected from an unlocked snapshot without queueing for the row lock. At 10 bidders it loses, spending about 3 wasted attempts per accepted bid on conflicts and retry tail latency.
- What experiment would confirm this explanation rather than just fit it?
- Which workload changes would flip or erase each result?
  - bidders who aim above the minimum
  - a larger `DB_MAX_CONNS` (there were 465k empty-acquire waits at w200)
  - more API instances
  - a different jitter cap or attempt limit
- Why might R15's hybrid path not simply get the best of both?
