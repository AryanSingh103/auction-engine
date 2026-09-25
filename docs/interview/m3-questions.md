# Milestone 3 interview questions

Generated from the M3 diff (`git diff bd604d3..HEAD`) by the interview-questions subagent. Answer in your own words, whenever you like (below each question, or in chat); Claude then grades the answers and points out where they are wrong or vague.

## 1. The token bucket and fail-open
*Files: `internal/ratelimit/ratelimit.go` (tokenBucket script), `internal/httpapi/ratelimit.go` (rateLimitBids)*

Your token bucket script reads `TIME` inside Lua, refills based on elapsed time, and writes back `tokens` and `ts`.
- Explain exactly which race you'd get if the same logic were written in Go as HMGET followed by HSET, even with Redis's clock.
- Why does using Redis `TIME` instead of the caller's clock matter here?
- When Redis is *slow* rather than down, what does a bidder actually experience under your 200 ms timeout and fail-open policy?
- An attacker can mint unlimited `X-User-ID` values anyway. Is fail-open giving up any real protection? What would you change first if this had to stop abuse rather than honest mistakes?

## 2. The version-guarded cache
*Files: `internal/cache/cache.go` (putIfNewer, Version), `internal/auction/service.go` (PlaceBid, refreshCache)*

- Walk through the interleaving that `putIfNewer` exists to prevent:
  1. Reader A misses and loads S1 from Postgres.
  2. A bid commits and `refreshCache` stores S2.
  3. A's `Put` lands.

  Show why a plain SET, or a DEL on write, still loses here.
- Prove that `2*head + closed` strictly increases across every state change. What would break the proof: a non-monotonic bid id sequence, a bid on a closed auction, or M5 closing an auction with no bids (compare version 0 with version 1)?
- Why does the refresh re-read Postgres after COMMIT instead of writing the transaction's view from inside the transaction? What exactly goes wrong in each alternative if the transaction rolls back, or if the commit fails after the write?

## 3. Snapshot and stream disagreeing
*Files: `internal/httpapi/live.go` (handleAuctionLive join-before-snapshot, RunLiveSync), `internal/live/bus.go` (BidMessage)*

A client connects to instance B while bids are being accepted on instance A. You Join the room before reading the snapshot, and the snapshot comes through `GetAuction`, which can be served from the cache.
- Build an interleaving where a queued bid message and the snapshot disagree. Include:
  - the case where A's `refreshCache` failed but its publish succeeded
  - the case where two bids on different instances are published out of order
- For each case, show what the `prev_bid_id` rule does: apply, ignore, or re-read.
- Can the client get stuck re-reading a stale cached head until the TTL expires?
- `RunLiveSync` reads through the same cache. What does that do to your claim that sync bounds staleness?

## 4. Dropping slow clients, and what really protects the stream
*Files: `internal/live/hub.go` (Broadcast, Close), `internal/httpapi/router.go` (live route outside requestTimeout), `docs/open-questions.md` (R12)*

`Hub.Broadcast` runs under the hub mutex and uses a non-blocking send, dropping any subscriber whose buffer is full. The connection loop then closes with 1013, or with 1001 on shutdown.
- Why is dropping the client better than blocking or growing the buffer without limit? What does the client lose by reconnecting instead of catching up?
- The live route sits outside the request-deadline middleware. Your first theory was that the server's `WriteTimeout` would kill the stream, and a mutation test showed that net/http clears deadlines on hijack. What does that tell you about how you verified R12?
- What protects a hijacked connection from a peer that stops reading, now that the server's timeouts no longer apply?
- What would change if you moved to SSE or HTTP/2?

## 5. A 30-second Redis outage
*Files: `docs/decisions/017-redis-as-accelerator.md`, `internal/auction/service.go` (PlaceBid post-commit block), `internal/live/bus.go` (Forward)*

Redis goes away for 30 seconds and then comes back, and nothing in `/readyz` notices. Trace, feature by feature, what users and operators see during and after the outage:
- bid latency with the limiter's timeout
- `GET /auctions/{id}` paying a failed Get plus a failed Put on top of Postgres
- bids published during the gap, which pub/sub delivers at most once and go-redis can't replay after resubscribing
- cache entries that survived the blip, or were evicted by `allkeys-lru`

Then:
- `PlaceBid` refreshes and publishes using the request's `ctx`. What happens if the bidder disconnects right after COMMIT?
- Under "Redis is an accelerator, never a source of truth", which of these is only a latency or freshness cost? Is there any case where a user sees an *incorrect* state rather than a stale one?
