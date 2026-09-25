# 020. Live updates: per-instance hubs fed by Redis pub/sub

## Context
Browsers need live prices from whichever API instance they're connected to. The brief asks for per-connection buffers that drop slow clients.

## Decision
- After commit: refresh the cache, then publish the bid to `auction:{id}:events`. Each instance `PSUBSCRIBE`s and broadcasts to its local room.
- Each connection has a bounded buffer. When it's full, the hub **drops that client** (1013 "try again later") instead of blocking the room.
- Each connection **joins before reading the snapshot**, then gets bids and a periodic `sync` of the head.
- **Pub/sub is lossy (R8).** Clients apply a bid only if its `prev_bid_id` equals their head; otherwise they re-read. The sync bounds staleness if the last bid is lost.
- The route sits outside the request deadline (mutation-tested). On shutdown, streams close with 1001.

## Alternatives considered
- **Redis Streams / Kafka for fan-out:** durable, but the bid chain already makes loss detectable, so plain pub/sub suffices.
- **Sticky sessions without cross-instance fan-out:** bids through another instance would never arrive.

## Consequences
One Redis connection per instance receives all auctions' events, fine at this scale. Delivery is at-most-once; correctness comes from resync.
