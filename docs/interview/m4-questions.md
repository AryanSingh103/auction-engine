# Milestone 4 interview questions

Generated from the M4 diff (`git diff 39d1b8d..HEAD`) by the interview-questions subagent. Answer in your own words, whenever you like (below each question, or in chat); Claude then grades the answers and points out where they are wrong or vague.

## 1. One publisher per batch, and the relay that does not know it is dead
*Files: `internal/outbox/relay.go` (PublishBatch, Run), `internal/outbox/kafka.go`, `internal/config/relay.go` (RELAY_PUBLISH_TIMEOUT vs DB_IDLE_IN_TX_TIMEOUT), `docs/decisions/022-outbox-relay.md`*

Each batch takes `pg_try_advisory_xact_lock`, reads the oldest unpublished rows in id order, waits for Kafka acks with the transaction still open, marks the rows published, and commits. The comment in `kafka.go` says a `Publish` may outlast its context, because franz-go cannot fail a batch that has already been sent.
- While relay A waits for acks, its session is idle in transaction. Construct the interleaving in which A's session is killed by `idle_in_transaction_session_timeout`, relay B takes the lock and publishes the same rows, and A's in-flight produce still lands. What does a consumer of one auction's partition see, in what order, and does anything in the idempotent producer prevent it?
- ADR 022 rejects a session-level lock because of zombie holders. Is the transaction-level lock actually immune to the zombie problem, or does it only narrow the window? What would a fencing token look like here?
- The ordering argument relies on one auction's events being inserted under its row lock, so their ids follow commit order. Identity values are allocated at insert time, not at commit. Show why the argument still holds for one auction, and describe the cross-auction case where a lower id commits after a higher one is already published. Why is that not a bug?
- Why does the relay fail the batch when `RowsAffected` differs from the number of ids, instead of committing what it marked? Under what conditions could that check ever fire?

## 2. Closing an auction, and what the schema refuses
*Files: `internal/auction/close.go` (CloseAuction), `migrations/00013_auction_close.sql` (AE012, AE013, AE014, outbox_one_close_per_auction), `migrations/00014_invoices.sql` (AE015)*

`CloseAuction` locks the auction row, reads `clock_timestamp()`, sets `status = 'closed'`, and inserts one `auction_closed` outbox event. A deferred constraint trigger (AE013) rejects the commit if the event is missing. A BEFORE INSERT trigger (AE014) rejects the event if the auction is not closed. A partial unique index allows at most one close event per auction.
- Walk through a bid whose transaction starts 5 ms before `end_at` and blocks on the row lock, while `CloseAuction` holds it and commits. Which clock reads happen, in what order, and which guard rejects the bid if the Go check were removed? What changes if either path used `now()`?
- AE013 is deferred to commit, but AE014 fires immediately. Why must each one be that way? What breaks if you swap them, or make both immediate?
- Two closers call `CloseAuction` on the same auction at once. Which mechanism stops a second close event: the row lock, the status check, AE010, or the partial unique index? If the row lock were removed by mistake, which of those would still hold, and what error would the loser see?
- AE015 checks an invoice against the auction's current head without locking the auction row. Why is that safe here, and which earlier guards is that safety borrowing from?

## 3. Exactly one charge, from an at-least-once stream
*Files: `internal/settlement/settler.go` (Settle, ensureInvoice, markPaid, markDeclined), `internal/paysim/store.go`, `internal/paysim/handler.go`, `internal/invariants/invariants.go` (Drained checks), `docs/decisions/023-payment-simulator.md`, `docs/decisions/024-settlement.md`*

The idempotency key is `invoice-<id>`. A 5xx or a timeout is an unknown outcome, and it leaves the invoice `pending`. A 402 marks it `failed`. The simulator sometimes charges and then answers 500.
- A consumer group rebalance hands a partition to settler B while settler A is still inside `Settle` for the same auction. A's first attempt charges but its response is lost. B's attempt gets the replayed charge and marks the invoice paid. Trace both through `markPaid`. Can this ever produce two charges, or an invoice that disagrees with the provider?
- ADR 024 rejects keys derived from the event id. Why is the invoice id better than the event id? Would `auction-<id>` have been better or worse still, and what would have to change about invoices for the invoice-id key to become unsafe?
- AE016 allows `failed -> pending` "for a replay after a dead-lettered settlement", yet a dead-lettered invoice stays `pending`, and only a 402 makes it `failed`. When is that transition actually used? If an operator replays a declined invoice, what does the provider return, and what does the settler do with it?
- "No charge without a paid invoice" runs only in drained mode. Describe the exact state in which it would report a false violation under the safety checks. How would you establish that the system is really drained before running it, and what race could invalidate the result?

## 4. Retries, full jitter and the circuit breaker
*Files: `internal/settlement/settler.go` (charge, backoff), `internal/breaker/breaker.go`, `internal/settlement/consumer.go` (handle, deadLetter), `cmd/settler/main.go`*

`charge` retries unknown outcomes with full-jitter backoff up to `PAYMENT_MAX_ATTEMPTS`. The breaker counts only unknown outcomes as failures. While the breaker is open, the settler sleeps without using up attempts, and the consumer handles one event at a time in partition order.
- Compare full jitter with exponential backoff without jitter, and with "equal jitter". What problem does full jitter solve? With only one serial consumer per process, is that problem even present here, and when would it become real?
- A declined card and a 422 key reuse both count as a breaker success. Justify that. What would go wrong if every non-2xx counted as a failure?
- The provider is down for an hour. Describe what each settler instance does minute by minute: the breaker state, the partitions it owns, the consumer group's view of it, and what piles up where. Does anything ever reach the DLQ? Is that the behavior you want?
- Each process has its own breaker. With three settler instances, how many calls reach a recovering provider per half-open window, and what happens when the half-open trial is one of the simulator's 5-second hangs?

## 5. Shedding on saturation, not lag
*Files: `internal/httpapi/shed.go`, `internal/httpapi/router.go` (middleware order), `internal/config/config.go` (BID_MAX_IN_FLIGHT), `internal/metrics/workers.go`, `docs/decisions/025-load-shedding.md`, `docs/open-questions.md` (R4)*

Bids beyond `BID_MAX_IN_FLIGHT` (2 × `DB_MAX_CONNS`) get an immediate 503 with `Retry-After: 1`. Shedding runs before the rate limiter. Consumer lag drives alerts only, and you dropped the lag-based relay throttle from the option the owner chose.
- Why 2 × the pool size rather than 1 ×, or something derived from latency? What does a bid admitted as slot `DB_MAX_CONNS + 1` experience, and what bounds its wait?
- Shedding before the rate limiter means a shed request costs no tokens. What does an abusive client that ignores `Retry-After` do to honest bidders on the same instance? Would putting the limiter first be better, and what would it cost?
- A fixed `Retry-After: 1` sent to thousands of shed clients at once: what traffic pattern follows? How would you change the response, or the client, to avoid it?
- You argued that throttling the relay on consumer lag only moves the backlog into the outbox table. Steelman the opposite position: what does an unthrottled relay cost Kafka, Postgres and the settlers during a long payment outage? Which metric would you alert on first, and at what threshold?
