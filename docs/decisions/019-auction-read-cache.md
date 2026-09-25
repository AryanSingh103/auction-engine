# 019. Version-guarded read cache for auction state

## Context
The hottest read is `GET /auctions/{id}`. Plain cache-aside has a race: a slow reader can cache state older than a newer write, which then stays stale until the TTL.

## Decision
- **Cache-aside for reads, plus a post-commit refresh on every accepted bid.** The refresh re-reads Postgres, so it can never cache uncommitted state.
- **Every write carries a version,** `2 × head bid id + closed`, which strictly increases because bids are append-only and status only goes open→closed. A Lua script stores only newer versions, so writes can land in any order and the cache never moves backwards. A mutation test shows the stale write wins without it.
- **TTL** (`AUCTION_CACHE_TTL`, 10s) bounds staleness if a refresh is lost.
- **Only display reads use the cache.** The bid path reads Postgres under the row lock.

## Alternatives considered
- **Invalidate (DEL) on write:** it has the same stale-refill race.
- **Write-through from inside the transaction:** that caches state that might roll back.
- **No cache:** the brief requires one.

## Consequences
One extra key read per accepted bid. M5 must bump the version when closing.
