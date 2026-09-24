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
Messages published while an instance or client is disconnected are lost. WebSocket clients need a state snapshot on connect, plus per-auction sequence numbers so they can detect gaps and resync.

## R9. Benchmark honesty (M2)
Numbers are measured inside the colima VM (4 vCPU / 6 GiB), not on bare metal, and `docs/benchmarks.md` must say so. Optimistic locking on a single hot row is expected to lose to pessimistic locking because of retry storms. Report whatever is actually measured.

## R10. AWS cost (M3.5, M6)
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
Found in the M0 review:
- The global `WriteTimeout` kills long-lived streams. Clear it per connection with `http.ResponseController(w).SetWriteDeadline(time.Time{})`, which works through chi's wrapper because it implements `Unwrap`.
- `Shutdown` does not track hijacked connections. Use `RegisterOnShutdown` to send close frames.
- `recoverer` writes a 500 after a panic, even on a hijacked connection.
- The request log's status is meaningless for upgraded connections.

## R13. Client-supplied request IDs (M6)
Found in the M0 review. chi's `RequestID` trusts an inbound `X-Request-Id` verbatim, bounded only by the 1 MB header limit. Clients can forge or collide correlation IDs and bloat logs. The API is not publicly reachable before M6. Decide then: use the ALB's `X-Amzn-Trace-Id`, or validate and cap the inbound value, or always generate our own.

## R14. Findings from the M1 review deferred to M2/M5
- **M2 load generator:** a 503 means "outcome unknown", not "not accepted". The generator must reconcile by idempotency key, re-sending or querying, before comparing its accepted-count against the database. Otherwise the chain-length assertions will mismatch spuriously.
- **M5 (anti-snipe moves `end_at`):** the invariant checks judge old bids by the auction's *current* `min_increment` and `end_at`. Once `end_at` can change, record on each bid the window end (and required minimum) it was judged against, and check against those.
- **Lock order (all milestones):** a bid takes the auction row lock first, then FK share locks on `users` and its own `bids` rows. Any new code path that locks several of these must lock the auction first, or it can deadlock against the bid path.
