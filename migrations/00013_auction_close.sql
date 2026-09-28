-- +goose Up
-- Closing an auction (M4 settlement consumes the event; M5's closer is the
-- normal caller). Closing sets status = 'closed' and writes exactly one
-- auction_closed outbox event, in one transaction:
--   AE012: an auction cannot be closed before its end_at (clock_timestamp(),
--          the database clock, never the transaction start: R1).
--   AE013: closing without writing the auction_closed event fails at COMMIT
--          (deferred, like AE007 for bids), so settlement always hears of it.
--   AE014: an auction_closed event can only be written for a closed auction.
--   outbox_one_close_per_auction: at most one auction_closed per auction.

ALTER TABLE outbox DROP CONSTRAINT outbox_event_type_check;
ALTER TABLE outbox ADD CONSTRAINT outbox_event_type_check
    CHECK (event_type IN ('bid_placed', 'auction_closed'));

CREATE UNIQUE INDEX outbox_one_close_per_auction ON outbox (auction_id)
    WHERE event_type = 'auction_closed';

-- The M1 guard (00010) plus AE012. The earlier checks are unchanged.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION auctions_update_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.status IS DISTINCT FROM OLD.status
       AND NOT (OLD.status = 'open' AND NEW.status = 'closed') THEN
        RAISE EXCEPTION 'auction % cannot change status from % to %', OLD.id, OLD.status, NEW.status
            USING ERRCODE = 'AE010';
    END IF;

    IF OLD.status = 'open' AND NEW.status = 'closed' AND clock_timestamp() < OLD.end_at THEN
        RAISE EXCEPTION 'auction % cannot close before its end (%)', OLD.id, OLD.end_at
            USING ERRCODE = 'AE012';
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

-- +goose StatementBegin
CREATE FUNCTION auctions_close_requires_outbox() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM outbox WHERE auction_id = NEW.id AND event_type = 'auction_closed') THEN
        RAISE EXCEPTION 'auction % was closed without an auction_closed event', NEW.id USING ERRCODE = 'AE013';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER auctions_close_requires_outbox AFTER UPDATE OF status ON auctions
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW
    WHEN (OLD.status = 'open' AND NEW.status = 'closed')
    EXECUTE FUNCTION auctions_close_requires_outbox();

-- +goose StatementBegin
CREATE FUNCTION outbox_close_requires_closed_auction() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM auctions WHERE id = NEW.auction_id AND status = 'closed') THEN
        RAISE EXCEPTION 'auction_closed event for auction %, which is not closed', NEW.auction_id
            USING ERRCODE = 'AE014';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER outbox_close_requires_closed_auction BEFORE INSERT ON outbox
    FOR EACH ROW
    WHEN (NEW.event_type = 'auction_closed')
    EXECUTE FUNCTION outbox_close_requires_closed_auction();

-- +goose Down
DROP TRIGGER outbox_close_requires_closed_auction ON outbox;
DROP FUNCTION outbox_close_requires_closed_auction();
DROP TRIGGER auctions_close_requires_outbox ON auctions;
DROP FUNCTION auctions_close_requires_outbox();

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION auctions_update_guard() RETURNS trigger
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

DROP INDEX outbox_one_close_per_auction;
ALTER TABLE outbox DROP CONSTRAINT outbox_event_type_check;
ALTER TABLE outbox ADD CONSTRAINT outbox_event_type_check CHECK (event_type IN ('bid_placed'));
