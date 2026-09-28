# 024. Settlement: idempotent per invoice, pending until the provider is definite

## Context
Every closed auction's winner must be charged exactly once (invariant 5), from an at-least-once event stream and through a provider that fails, loses responses and hangs (ADR 023).

## Decision
- **One consumer group** handles `auction_closed` events in partition order, and marks each offset only after handling it.
- **The invoice is built from the closed auction row,** not the event: `INSERT … ON CONFLICT (auction_id) DO NOTHING`.
- **The idempotency key is `invoice-<id>`**, the same for every attempt, redelivery and instance.
- **Invoice statuses:**
  - `paid`: a charge is confirmed
  - `failed`: only after a 402, where no charge exists
  - `pending`: an unknown outcome, never marked failed, because a lost response may have charged; after the retries it is dead-lettered for replay
- **Full-jitter backoff, and a circuit breaker on unknown outcomes only.** While the breaker is open, settlement pauses; it does not dead-letter everything.
- Database errors retry the event, blocking its partition.

## Alternatives
- **Marking `failed` after the retries run out:** would lie when a charge exists.
- **Keying on the event id:** a republished event would charge twice.

## Consequences
One slow invoice blocks its partition. The key test is to randomize the key, which 6 tests catch.
