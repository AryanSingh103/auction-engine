# Milestone 1 interview questions

Generated from the M1 diff (`git diff 2211031..HEAD`) by the interview-questions subagent. Answer in your own words, whenever you like (below each question, or in chat); Claude then grades the answers and points out where they are wrong or vague.

## 1. Lock waits, snapshots and three clocks
*Files: `internal/auction/service.go` (placeBidTx steps 2–4, lockAuction), `migrations/00006_bid_guard_trigger.sql`, `migrations/00004_bids.sql`, ADR 007*

- A bid's `SELECT ... FOR UPDATE` blocks behind another bid, and that bid commits. Under READ COMMITTED, which version of the auction row does the waiting transaction get back, and what does Postgres do to produce it?
- ADR 007 says a `clock_timestamp()` in that statement's target list came back 660ms stale. Explain why.
- One accepted bid now involves three separate `clock_timestamp()` calls: your step-4 read, the `ts` read inside `bids_guard`, and the `created_at` default. Put them in order.
- Can the invariant checker's "bids accepted inside the auction window" check ever report a false violation, or miss a real one?

## 2. Idempotency at the edges
*Files: `internal/auction/service.go` (PlaceBid, replay, step 3), `internal/httpapi/auctions.go` (writeServiceError), ADR 009*

- Your 503 response says "it is safe to retry with the same Idempotency-Key". Construct a case where the request deadline fires around COMMIT, so the client gets 503 but the bid may be durable. What does the retry return, and is that correct?
- Only accepted bids store their key. Give a real sequence where one key first gets a rejection and later gets a 201. Does that break idempotency as a client would understand it?
- Why does the `bids_idempotency` unique-violation fallback in `PlaceBid` never fire for same-key requests on the same auction? What would change if the lookup moved back in front of the lock?

## 3. What each schema layer actually buys you
*Files: `migrations/00004_bids.sql`, `00005_outbox.sql`, `00006_bid_guard_trigger.sql`, `00007_bid_requires_outbox.sql`, ADR 008*

- `bids_guard` already re-locks the auction row and rejects a stale head (AE004). What does `UNIQUE NULLS NOT DISTINCT (auction_id, prev_bid_id)` protect against that the trigger does not?
- What exact corruption becomes possible if you drop `NULLS NOT DISTINCT`, or the composite FK `(auction_id, prev_bid_id)`?
- Why does "no bid without its event" need a deferred constraint trigger rather than a foreign key?
- Name at least two ways that invariant can still be broken after commit, or bypassed, with the schema as written. Would the invariant checker catch each one?

## 4. Does the 1000-bid test prove what it claims?
*Files: `internal/auction/service_test.go`, `internal/invariants/invariants.go`, `invariants_test.go`, `migrations/schema_test.go`*

- Suppose you delete the `FOR UPDATE` from `lockAuction` and rerun `TestConcurrentBidsNoLostUpdate`. Which assertion fails first? What error do the losing bids get, and which HTTP status would a real client see? (Follow AE004 through `mapDBError` and `writeServiceError`.)
- With a pool of 20 connections, how much concurrency does the "1000 concurrent bids" test really exercise? What stops it from passing just because the bids happened to run nearly one after another?
- Describe one realistic bug in the bid path that this test, the schema tests and `TestEachCheckDetectsItsViolation` would all let through.

## 5. Two copies of the rules, and changing them
*Files: `internal/auction/service.go` (mapDBError), `internal/httpapi/auctions.go` (serviceErrors), `cmd/migrate/main.go`, `internal/testdb/testdb.go`, ADRs 006 and 011*

The bid rules are written twice: in Go (`validateBid`) and in SQL (`bids_guard`). Migrations run as a separate goose step that must stay backward-compatible with the API version still running.
- Walk through changing one rule, say the minimum increment becomes a percentage, from migration through API rollout.
- During the window where Go and SQL disagree, what does a client get in each direction (Go stricter vs SQL stricter), and how would you notice in production?
- The test harness migrates a template database once, straight to the latest version, and clones it for every test. What about your migrations does that setup never test?
