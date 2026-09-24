# 013. Prometheus metrics design

## Context
Metrics must explain *why* latency moves (lock contention, pool exhaustion), not just that it moved.

## Decision
- **Separate listener** (`METRICS_ADDR`, `:9091`): `/metrics` is never on the public port, so M6's ALB can't expose it. It shuts down after the API drains, so the drain itself stays observable.
- **Own registry** (not the global default), so tests get isolated instances.
- **HTTP** request counts and durations, labelled by chi's **route pattern** (never the raw path) and a fixed method set (others become `OTHER`): bounded cardinality.
- **Bid path:** `bids_total{outcome}` (every outcome pre-created at 0), `bid_lock_wait_seconds`, `bid_transaction_seconds`.
- **Pool:** a collector reads `pgxpool.Stat()` at scrape time. `empty_acquires_total` shows when the pool, not Postgres, is the bottleneck.
- **Buckets:** 0.5ms to 10s, exponential. The defaults start at 5ms, where a bid would already be past the median.
- **The domain stays library-free:** `internal/auction` reports through an `Observer` interface, and `internal/metrics` implements it.

## Alternatives considered
- **OpenTelemetry metrics:** vendor-neutral, but more machinery than one Prometheus scrape needs.
- **Summaries instead of histograms:** they can't be aggregated across API instances (M3).

## Consequences
Client- and server-side latency can be compared. New outcomes need an `AllOutcomes` entry.
