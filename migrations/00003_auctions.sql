-- +goose Up
-- One auction of one item. Money is integer cents (BIGINT), never floating
-- point. The current_* columns describe the head of the accepted-bid chain
-- and are only ever changed by the bid path while it holds this row's lock.
CREATE TABLE auctions (
    id                BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    item_id           BIGINT      NOT NULL REFERENCES items (id),
    start_at          TIMESTAMPTZ NOT NULL,
    end_at            TIMESTAMPTZ NOT NULL,
    starting_price    BIGINT      NOT NULL CHECK (starting_price > 0),
    min_increment     BIGINT      NOT NULL CHECK (min_increment > 0),
    -- NULL until the first bid is accepted. current_bid_id gets its foreign
    -- key in the bids migration, since bids does not exist yet.
    current_price     BIGINT,
    current_leader_id BIGINT      REFERENCES users (id),
    current_bid_id    BIGINT,
    -- Closing (and the winner columns) arrive with the closer in milestone 5.
    status            TEXT        NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'closed')),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT auctions_window CHECK (end_at > start_at),
    -- The head is all-or-nothing: a price without a leader (or a leader
    -- without a bid) is a half-applied update.
    CONSTRAINT auctions_head_all_or_none CHECK (
        (current_price IS NULL AND current_leader_id IS NULL AND current_bid_id IS NULL)
        OR (current_price IS NOT NULL AND current_leader_id IS NOT NULL AND current_bid_id IS NOT NULL)
    ),
    CONSTRAINT auctions_price_floor CHECK (current_price >= starting_price)
);

-- +goose Down
DROP TABLE auctions;
