-- +goose Up
-- The auction head (current_bid_id, current_leader_id, current_price) must
-- describe one real bid exactly. Before this, only current_bid_id was tied to
-- a bid, so the leader or price columns could drift from it. The closer
-- (milestone 5) will name the winner from these columns, so they must be
-- trustworthy (invariant 2). Found by the milestone 1 adversarial review.
ALTER TABLE bids
    ADD CONSTRAINT bids_head_target UNIQUE (auction_id, id, user_id, amount);

ALTER TABLE auctions
    DROP CONSTRAINT auctions_head_is_own_bid,
    ADD CONSTRAINT auctions_head_is_own_bid
        FOREIGN KEY (id, current_bid_id, current_leader_id, current_price)
        REFERENCES bids (auction_id, id, user_id, amount);

-- +goose Down
ALTER TABLE auctions
    DROP CONSTRAINT auctions_head_is_own_bid,
    ADD CONSTRAINT auctions_head_is_own_bid FOREIGN KEY (id, current_bid_id)
        REFERENCES bids (auction_id, id);
ALTER TABLE bids DROP CONSTRAINT bids_head_target;
