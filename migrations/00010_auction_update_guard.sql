-- +goose Up
-- Restricts how an auction row may change:
--   AE010: status only moves open -> closed; a closed auction never reopens
--          (reopening would let bids be accepted after close, invariant 4).
--   AE011: the head only advances one link along the bid chain, to a bid
--          whose predecessor is the current head. It can never move backwards
--          or jump to an unrelated bid.
-- Found by the milestone 1 adversarial review.
-- +goose StatementBegin
CREATE FUNCTION auctions_update_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.status IS DISTINCT FROM OLD.status
       AND NOT (OLD.status = 'open' AND NEW.status = 'closed') THEN
        RAISE EXCEPTION 'auction % cannot change status from % to %', OLD.id, OLD.status, NEW.status
            USING ERRCODE = 'AE010';
    END IF;

    IF NEW.current_bid_id IS DISTINCT FROM OLD.current_bid_id THEN
        IF NEW.current_bid_id IS NULL OR NOT EXISTS (
            SELECT 1 FROM bids
            WHERE id = NEW.current_bid_id
              AND auction_id = NEW.id
              AND prev_bid_id IS NOT DISTINCT FROM OLD.current_bid_id
        ) THEN
            RAISE EXCEPTION 'auction % head may only advance to a bid that extends bid %', OLD.id, OLD.current_bid_id
                USING ERRCODE = 'AE011';
        END IF;
    END IF;

    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER auctions_update_guard BEFORE UPDATE ON auctions
    FOR EACH ROW EXECUTE FUNCTION auctions_update_guard();

-- +goose Down
DROP TRIGGER auctions_update_guard ON auctions;
DROP FUNCTION auctions_update_guard();
