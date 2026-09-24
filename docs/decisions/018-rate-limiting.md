# 018. Per-user token bucket in Redis, failing open

## Context
The brief asks for Redis token-bucket rate limiting that returns 429 with Retry-After. Per-process limits would multiply with the instance count.

## Decision
- **One bucket per user**, shared by all instances, on bid placement only (a chi route group). Reads are cached and cheap.
- **One Lua script** refills and takes a token atomically, using Redis's own clock (`TIME`), so skewed instance clocks can't disagree. Idle buckets expire. Tested: 200 concurrent requests against a burst of 10 let exactly 10 through.
- A limited request gets 429 `rate_limited` with `Retry-After` rounded up to whole seconds.
- **Fail open:** on a Redis error the bid proceeds, and the error is counted. The limit protects capacity; Postgres protects correctness.
- Configured by `RATE_LIMIT_BIDS_PER_SECOND` and `RATE_LIMIT_BID_BURST`.

## Alternatives considered
- **In-process limiter:** it multiplies with instance count.
- **A fixed-window counter (`INCR` + `EXPIRE`):** simpler, but it allows 2× bursts at window edges.
- **Fail closed:** a Redis outage would block all bidding.

## Consequences
The key is the claimed `X-User-ID`, and there is no authentication (ADR 010). It curbs honest clients' mistakes, not attackers who rotate ids.
