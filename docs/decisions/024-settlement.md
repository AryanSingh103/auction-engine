# 024. Settlement: idempotent per invoice, pending until the provider is definite

## Context
Charge each closed auction's winner exactly once (invariant 5), from an at-least-once stream, through a provider that fails, loses responses and hangs (ADR 023).

## Decision
- **One consumer group** handles `auction_closed` events in partition order, and marks each offset only after handling it.
- **The invoice is built from the closed auction row,** not the event: `INSERT … ON CONFLICT (auction_id) DO NOTHING`.
- **The idempotency key is `invoice-<id>`**, the same for every attempt, redelivery and instance.
- **Invoice statuses:**
  - `paid`: a charge is confirmed
  - `failed`: only after a 402, where no charge exists
  - `pending`: an unknown outcome, never marked failed, because a lost response may have charged; after the retries it is dead-lettered for replay
- **Full-jitter backoff, and a circuit breaker on unknown outcomes only.** While the breaker is open, settlement pauses; it does not dead-letter everything.
- Database errors retry the event.

## Alternatives
- **`failed` after the retries:** would lie when a charge exists.
- **Event-id keys:** a republish would charge twice.

## Consequences
One slow invoice blocks its partition. Randomizing the key fails 6 tests.
