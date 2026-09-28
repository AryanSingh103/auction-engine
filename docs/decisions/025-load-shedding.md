# 025. Shed bids on API saturation, not on consumer lag

## Context
The brief sheds load when Kafka consumer lag passes a threshold. Bids do not depend on settlement, so that would stop bidding whenever the payment provider is slow. The owner chose saturation-based shedding instead (R4, 2026-09-27).

## Decision
- **A per-instance concurrency cap on bid requests** (`BID_MAX_IN_FLIGHT`, 2 × `DB_MAX_CONNS`). Beyond it, a bid gets 503 `overloaded` with `Retry-After: 1` immediately, before the rate limiter.
- **Shedding is counted** in `bid_shedding_decisions_total`. Measured with a cap of 2: of 200 concurrent bids, 145 were shed, and the metric agreed.
- **Consumer lag drives alerts only.** Throttling the relay on lag (part of the R4 option) was dropped, pending the owner's OK: Kafka is the buffer, so throttling would only move the backlog into the outbox table and delay unrelated bid events.

## Alternatives
- **Shedding on lag (the brief):** couples unrelated paths.
- **Queueing on the pool:** bids get slow, then time out.
- **Adaptive limits (AIMD):** better, more to explain.

## Consequences
The cap is static and per instance. Benchmarks lift it, as they lift the rate limits.
