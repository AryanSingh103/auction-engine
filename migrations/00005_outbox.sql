-- +goose Up
-- Transactional outbox: events are written in the same transaction as the
-- state change that produced them, and published to Kafka later by a
-- separate poller (milestone 4). Rows are marked published, never deleted,
-- because a bid must always have its event (invariant 6; see 00007).
CREATE TABLE outbox (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    -- The Kafka message key (docs/PROJECT_BRIEF.md: keyed by auction_id).
    auction_id   BIGINT      NOT NULL REFERENCES auctions (id),
    event_type   TEXT        NOT NULL CHECK (event_type IN ('bid_placed')),
    payload      JSONB       NOT NULL,
    -- The bid this event reports. UNIQUE: one event per bid.
    bid_id       BIGINT      UNIQUE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    published_at TIMESTAMPTZ,

    -- Invariant 6, "no outbox event without its bid": the event's bid must
    -- exist and belong to the same auction.
    CONSTRAINT outbox_bid_of_auction FOREIGN KEY (auction_id, bid_id)
        REFERENCES bids (auction_id, id),
    -- bid_placed events always name their bid; other event types (added in
    -- later milestones) never do.
    CONSTRAINT outbox_bid_event_has_bid CHECK ((event_type = 'bid_placed') = (bid_id IS NOT NULL))
);

-- +goose Down
DROP TABLE outbox;
