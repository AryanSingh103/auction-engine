# 017. Redis as an accelerator, never a source of truth

## Context
M3 adds Redis for caching, rate limiting and pub/sub. The brief makes Postgres the single source of truth.

## Decision
Nothing in Redis is authoritative, so it runs with **no persistence** and a 128 MB `allkeys-lru` cap. Every call uses a short timeout (`REDIS_TIMEOUT`, 200 ms) with no retries, and every feature **degrades instead of failing**:
- **Cache miss or error:** read Postgres.
- **Limiter error:** allow the bid (ADR 018).
- **Lost pub/sub message:** clients detect the gap and resync from Postgres (ADR 019).

`/readyz` checks only Postgres. An instance without Redis can still serve correct answers, so the load balancer shouldn't remove it.

## Alternatives considered
- **Redis as a write-behind store for bids:** faster, but it breaks "Postgres is the source of truth" and invariant 7 across crashes.
- **Failing requests when Redis is down:** a cache outage would become a full outage.

## Consequences
The bid path never reads Redis, so the invariants don't depend on it. Redis failures show up as metrics, not errors: `rate_limit_decisions_total{result="error"}` and cache error counters.
