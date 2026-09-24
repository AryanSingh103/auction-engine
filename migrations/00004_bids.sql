-- +goose Up
-- Accepted bids only (rejected attempts have no side effects and are not
-- stored). Accepted bids on an auction form a singly linked chain via
-- prev_bid_id; see docs/decisions/008.
CREATE TABLE bids (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    auction_id      BIGINT      NOT NULL REFERENCES auctions (id),
    user_id         BIGINT      NOT NULL REFERENCES users (id),
    amount          BIGINT      NOT NULL CHECK (amount > 0),
    -- The bid this one outbid; NULL for the first bid of an auction.
    prev_bid_id     BIGINT,
    idempotency_key TEXT        NOT NULL CHECK (length(idempotency_key) BETWEEN 1 AND 255),
    -- clock_timestamp(), not now(): now() is the transaction START time, so
    -- a bid that waited on the auction lock would record a time from before
    -- it was actually accepted.
    created_at      TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),

    -- Target for the composite foreign keys below, so "a bid of THIS
    -- auction" can be enforced, not just "some bid".
    CONSTRAINT bids_auction_id_id UNIQUE (auction_id, id),
    -- The predecessor must be a bid on the same auction.
    CONSTRAINT bids_prev_same_auction FOREIGN KEY (auction_id, prev_bid_id)
        REFERENCES bids (auction_id, id),
    -- No fork: at most one bid may extend any given predecessor, and NULLS
    -- NOT DISTINCT (Postgres 15+) makes that "at most one first bid" too.
    -- Two concurrent bids that both read the same head can never both
    -- commit, so a lost update is impossible by construction.
    CONSTRAINT bids_chain_no_fork UNIQUE NULLS NOT DISTINCT (auction_id, prev_bid_id),
    -- Idempotency keys are scoped per user, as in Stripe's API.
    CONSTRAINT bids_idempotency UNIQUE (user_id, idempotency_key)
);

-- The auction's head must be one of its own bids.
ALTER TABLE auctions
    ADD CONSTRAINT auctions_head_is_own_bid FOREIGN KEY (id, current_bid_id)
        REFERENCES bids (auction_id, id);

-- +goose Down
ALTER TABLE auctions DROP CONSTRAINT auctions_head_is_own_bid;
DROP TABLE bids;
