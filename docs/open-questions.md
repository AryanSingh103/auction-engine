# Open questions and spec risks

Gaps in the brief's wording or mechanisms that would quietly break an invariant if nobody decided them. Each item is settled in the milestone shown and closed by an ADR in `docs/decisions/`. When one is resolved, mark it with the ADR number rather than deleting it.

## R1. `now()` vs `clock_timestamp()` in time checks (M1, M5): critical
**Resolved in M1 (ADR 007, 008).** Measured: a `clock_timestamp()` in the target list of a `FOR UPDATE` select is evaluated before the lock wait when the holder did not modify the row. The bid path reads the clock in a separate statement after the lock, the guard trigger does the same, and a test fails if either regresses. M5 must keep this for the closer.
In Postgres, `now()` returns the transaction's *start* time. Picture a bid transaction that starts before `end_at`, blocks on `SELECT ... FOR UPDATE` while the closer holds the row, and resumes after the close. It would still see `now() < end_at` and accept the bid, which violates invariant 4. Time checks made while holding the lock must use `clock_timestamp()`. The close-vs-bid race test must reproduce this exact interleaving.

## R2. Outbox publish ordering with multiple pollers (M4): critical
Keying Kafka messages by `auction_id` only preserves order if events are *produced* in order. If several pollers use `FOR UPDATE SKIP LOCKED`, poller A can lock event 1 and poller B event 2 of the same auction, and B may publish first. Options:
- a single publisher behind an advisory lock
- pollers sharded by `hash(auction_id)`
- per-auction sequence numbers, with consumers that tolerate reordering

## R3. Exactly one charge needs an idempotent payment provider (M4): critical
A payment call that times out has an unknown outcome. If we retry blindly, we can charge twice. Settlement idempotency on our side is necessary but not sufficient. The fake payment service must accept an idempotency key and return the original result on a repeat, as Stripe does. The plan is to use the invoice id as that key.

## R4. Load shedding keyed on consumer lag (M4): questionable spec
The brief sheds load when Kafka consumer lag passes a threshold. Bids don't depend on settlement, so rejecting bids because settlement is behind couples two unrelated paths. Proposal:
- shed on API-local saturation (requests in flight, DB pool wait time)
- use consumer lag to throttle the outbox publisher and raise alerts

To be discussed at M4.

## R5. Invariant wording gaps (M1)
**Resolved in M1 (ADR 011).** The first bid must be ≥ starting_price. No self-outbid. Zero bids means no winner and no invoice. No reserve price.
- An auction that closes with zero bids: no winner and no invoice? Invariant 1 then reads "at most one winner, and exactly one if there was an accepted bid".
- Must the first bid be `>= starting_price`, or `>= starting_price + min_increment`?
- May the current leader raise their own bid?
- Is there a reserve price? (Proposal: no.)

## R6. Identity (M1)
**Resolved in M1 (ADR 010).** `X-User-ID` header, with no real auth.
Bids, per-user rate limits (M3) and invoices all need a user, but the brief defines no authentication. Proposal: an `X-User-ID` header checked against `users`, documented explicitly as "no real auth, out of scope".

## R7. Invariant 6 in both directions (M1)
**Resolved in M1 (ADR 008).** A composite FK from outbox to bid, plus a deferred constraint trigger (AE007), plus invariant checks.
"No outbox event without its bid" needs a link from outbox to bid: a nullable `bid_id` foreign key with a unique constraint. The invariant checker should also query for bids that have no event.

## R8. Redis pub/sub is fire-and-forget (M3)
**Resolved in M3 (ADR 020).** Clients get a snapshot on connect, apply a bid only if its `prev_bid_id` equals their head (otherwise they re-read), and a periodic `sync` of the head bounds staleness if the last message is lost. The bid chain provides the sequence numbers, so no separate counter was needed.
Messages published while an instance or client is disconnected are lost. WebSocket clients need a state snapshot on connect, plus per-auction sequence numbers so they can detect gaps and resync.

## R9. Benchmark honesty (M2)
Numbers are measured inside the colima VM (4 vCPU / 6 GiB), not on bare metal, and `docs/benchmarks.md` must say so. Optimistic locking on a single hot row is expected to lose to pessimistic locking because of retry storms. Report whatever is actually measured.

## R10. AWS cost (M3.5, M6)
**Checked for M3.5 (2026-09-27):**
- A $10/month budget (`monthly-guard`) exists.
- The account is on the free plan, with $120 of credits until 2027-03-27.
- The trial used no NAT gateway and cost about $0.07/hour (ADR 021).
- It was destroyed the same day, and a direct check found nothing billable left.

Re-check all of this before M6, where RDS and an always-on ALB will cost money even when idle.

Before the first `terraform apply`:
- set a billing budget alarm
- avoid NAT gateways (about $30/month each)
- remember that the ALB and RDS cost money even when idle
- re-check the current free-tier terms

Any step that creates billable resources needs the owner's explicit go-ahead.

## R11. Handler deadlines shorter than WriteTimeout (M1)
**Resolved in M1.** The `requestTimeout` middleware, with `REQUEST_TIMEOUT < HTTP_WRITE_TIMEOUT` validated in config. A timed-out bid returns 503. Usually nothing was written, but if the deadline fires during COMMIT the bid may be durable. A retry with the same Idempotency-Key resolves either case (see R14).
Found in the M0 review. When `WriteTimeout` expires, net/http kills the connection silently. The handler's context is **not** cancelled, the handler runs to completion (holding a DB connection and row locks from M1 on), and the request log records the status the handler wrote (e.g. 200), not what the client saw (EOF). M1 needs a request-deadline middleware (`context.WithTimeout`, below `WriteTimeout`) that DB calls honor. `http.TimeoutHandler` is the alternative, but it buffers responses and breaks hijacking, so it is unsuitable once WebSockets arrive.

## R12. WebSocket traps in the M0 server setup (M3)
**Resolved in M3.** The live route sits outside the request deadline (mutation-tested). The hub closes streams with 1001 from `RegisterOnShutdown`. The recoverer skips writing a status on upgraded connections. The WriteTimeout item below turned out to be wrong.
Found in the M0 review:
- ~~The global `WriteTimeout` kills long-lived streams.~~ **Wrong, corrected in M3:** a WebSocket upgrade hijacks the connection, and `net/http` clears the connection's deadlines on hijack (`server.go`, `conn.hijackLocked`). A mutation test confirmed it. The real trap was the *request deadline middleware*, which does cancel the stream (also mutation-tested), so the live route sits outside it. `WriteTimeout` would matter for non-hijacked streams (SSE, HTTP/2).
- `Shutdown` does not track hijacked connections. Use `RegisterOnShutdown` to send close frames.
- `recoverer` writes a 500 after a panic, even on a hijacked connection.
- The request log's status is meaningless for upgraded connections.

## R13. Client-supplied request IDs (M6)
Found in the M0 review. chi's `RequestID` trusts an inbound `X-Request-Id` verbatim, bounded only by the 1 MB header limit. Clients can forge or collide correlation IDs and bloat logs. The API is not publicly reachable before M6. Decide then: use the ALB's `X-Amzn-Trace-Id`, or validate and cap the inbound value, or always generate our own.

## R14. Findings from the M1 review deferred to M2/M5
- **M2 load generator:** a 503 means "outcome unknown", not "not accepted". The generator must reconcile by idempotency key, re-sending or querying, before comparing its accepted-count against the database. Otherwise the chain-length assertions will mismatch spuriously.
- **M5 (anti-snipe moves `end_at`):** the invariant checks judge old bids by the auction's *current* `min_increment` and `end_at`. Once `end_at` can change, record on each bid the window end (and required minimum) it was judged against, and check against those.
- **Lock order (all milestones):** a bid takes the auction row lock first, then FK share locks on `users` and its own `bids` rows. Any new code path that locks several of these must lock the auction first, or it can deadlock against the bid path.

## R15. Hybrid bid path: reject doomed bids before the lock (from the M2 benchmark)
In M2's hot-auction runs, 73–98% of bids were "too low". The pessimistic path makes each of them wait for the auction row lock just to be refused. That is the *hypothesis* for why optimistic had 64% more throughput at 200 bidders, but pool queueing (20 connections for 200 workers) confounds it (docs/benchmarks.md, ADR 016). Proposal: an unlocked read first, rejecting `bid_too_low` immediately, and only then the full pessimistic path. It's safe because prices only rise: a stale snapshot understates the minimum, so a "too low" answer can never be wrong. Before adopting it, measure it with the same matrix, including a run with a larger `DB_MAX_CONNS` to separate lock queueing from pool queueing, and check that the pre-check doesn't reintroduce the idempotency race fixed in M1 (a replay must still be answered as a replay, not as "too low").

## R16. Findings from the M2 review deferred to later milestones
- **M5, closer vs the optimistic path:** a close landing between the unlocked read and the INSERT fails with AE001 from the guard. `OutcomeOf` would label that `guard_bug` (the alert-on-it outcome), although it's an expected race for the optimistic strategy. Make `IsExpectedGuardRejection` strategy-aware before M5 ships the closer.
- **Optimistic too-low minimum:** under optimistic, a too-low 409 reports the minimum from a possibly stale snapshot, which may be too low. The client's next bid can then be doomed too. It's correct, but wasteful.
- **Next benchmark round:** add a poll-interval flag to the load generator (or make leaders idle instead of polling) and show the results don't move with it; measure the load generator's own CPU; record Postgres CPU.

## R17. Findings from the M3 review deferred to later milestones
- **A Redis circuit breaker (M4).** When Redis black-holes (`docker compose pause redis`), every Redis call waits for its 200 ms timeout. The review measured a bid at about 0.7 s and 150 concurrent GETs at p50 1.6 s. The pool wait is now bounded (it was the worst part), but a breaker that skips Redis entirely while it's failing would remove the rest. M4 builds a circuit breaker for the payment service; reuse it here.
- **The rate limiter charges replays.** It runs before the idempotency lookup, so an honest retry of a timed-out bid can get 429 instead of its replay. Also, `allkeys-lru` eviction under memory pressure resets buckets to full. Both are acceptable for a capacity limit on honest clients (ADR 018); revisit if the limit ever has to stop abuse.
- **The cache version ignores `end_at` (M5, must fix).** `Version = 2*head + closed`. If M5's anti-snipe extends `end_at` in the same transaction as a bid, the head moves and the version grows, which is fine. But any change to `end_at` (or another displayed field) *without* a new bid would be rejected as "equal version", and the cache would keep the old value until its TTL. M5 must either make every `end_at` change go through a version bump or include an explicit version column.

## R18. Findings from the M3.5 trial deploy and review, deferred to M6
- **Instance termination does not drain tasks.** When the ASG terminates an instance (replacement, instance refresh, health replacement), ECS is not told. In the trial, the task got SIGTERM 26s before it was deregistered from the target group, followed by about 60s with no task at all. M6 needs these together:
  - an `aws_ecs_capacity_provider` with `managed_draining = "ENABLED"` and managed termination protection (with `protect_from_scale_in` on the ASG)
  - `max_size` of at least 2, so the task has somewhere to go
  - `name_prefix` plus `create_before_destroy` on the ASG, because a fixed name forces destroy-then-create, which is an outage
- **Deployment circuit breaker.** Enable `deployment_circuit_breaker { enable = true, rollback = true }`. Without it, a bad image makes ECS retry forever, and `wait_for_steady_state` hangs until Terraform times out.
- **AMI pinning.** Reading the AMI from SSM on every plan gives a launch-template diff whenever AWS publishes a new AMI, and it still never replaces the running instance. Pin the AMI ID in a variable and roll it on purpose, through instance refresh plus managed draining.
- **Secrets out of plain env vars.** `DATABASE_URL` holds the RDS password. Pass it through container `secrets` (`valueFrom` SSM SecureString or Secrets Manager), and grant the execution role read access to exactly that parameter. Otherwise it lands in the task definition and in state. State itself moves to an encrypted S3 backend with locking.
- **SSM access to instances.** Attach `AmazonSSMManagedInstanceCore` to the instance role, for Session Manager access without SSH or any inbound port. It runs over the existing 443 egress. Consider ECS Exec as well.
- **`drop_invalid_header_fields = true` on the ALB.** ADR 010 trusts an identity header, so the ALB should drop malformed header fields instead of forwarding them.
- **IAM propagation on a fresh account.** The first apply failed because the AutoScaling service-linked role was created 5s before the first launch. The role now exists in this account. Do not add an `aws_iam_service_linked_role` resource for it: creating it would fail because it already exists.
