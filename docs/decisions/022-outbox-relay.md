# 022. Outbox relay: one publisher per batch, via a transaction-level advisory lock

## Context
Outbox events must reach Kafka at least once, each auction's in order (R2). The brief's `SKIP LOCKED` pollers can publish one auction's events out of order.

## Decision
- Each batch is one transaction: `pg_try_advisory_xact_lock`, read unpublished rows in id order, produce, await acks, mark, commit. Any number of relays; one batch at a time.
- An auction's events are inserted under its row lock, so their ids follow commit order.
- franz-go (maintained, idempotent by default). Keyed by auction id; the value carries the event id.
- Tested: 4 relays deliver every event once, in order; without the lock, 5 of 5 runs duplicate.

## Alternatives
- **A session lock:** a zombie holder keeps publishing after its connection dies.
- **Sharded pollers:** more throughput, more moving parts.
- **Kafka transactions:** they do not cover Postgres.

## Consequences
At least once, and a duplicate can land after newer events (a relay whose session Postgres ended may still retry a sent batch). Consumers must be idempotent and drop ids at or below the last applied for that auction.
