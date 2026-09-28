# 022. Outbox relay: one publisher per batch, via a transaction-level advisory lock

## Context
Outbox events must reach Kafka at least once, each auction's in order (R2). The brief's `SKIP LOCKED` pollers can publish one auction's events out of order.

## Decision
- Each batch is one transaction: `pg_try_advisory_xact_lock`, read the oldest unpublished rows in id order, produce and wait for acks, mark them published, commit. Any number of relays can run, but only one batch runs at a time.
- An auction's events are inserted under its row lock, so their ids follow commit order.
- The client is franz-go (maintained, idempotent producer by default). Records are keyed by auction id, and the value carries the event id so consumers can deduplicate.
- Tests: 4 relays with concurrent bids deliver every event exactly once, in order. With the lock removed, 5 of 5 runs publish duplicates.

## Alternatives
- **A session lock:** a zombie holder keeps publishing after its connection dies.
- **Sharded pollers:** more throughput, more moving parts.
- **Kafka transactions:** they do not cover Postgres.

## Consequences
A crash between the acknowledgement and the commit republishes the batch, so consumers must be idempotent.
