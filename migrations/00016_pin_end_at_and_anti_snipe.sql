-- +goose Up
-- Milestone 5: anti-snipe, a pinned end_at, and a version for the cache.
-- Found by the M3 and M4 reviews (R17, R19). See docs/decisions/026.
--
--   version:       bumped by the trigger on every UPDATE, so the read cache
--                  can order any two states of an auction (R17).
--   extend_window, extend_by: anti-snipe. A bid accepted within
--                  extend_window of end_at moves end_at to
--                  bid time + extend_by (if that is later). Zero disables it.
--   closed_at:     set by the trigger to the database clock when the
--                  auction closes; the audit compares it with end_at.
--
-- New guards:
--   AE017: a new auction must start open, without a head, at version 0.
--   AE018: end_at only changes as the anti-snipe rule says, in the same
--          UPDATE that advances the head. So it only grows, only while
--          open, and never without a bid (the R19 early-close bypass).
--   AE019: a closed auction never changes again, and the fixed terms of
--          an auction (item, start, prices, anti-snipe) never change.

ALTER TABLE auctions
    ADD COLUMN version       BIGINT      NOT NULL DEFAULT 0,
    ADD COLUMN extend_window INTERVAL    NOT NULL DEFAULT '0',
    ADD COLUMN extend_by     INTERVAL    NOT NULL DEFAULT '0',
    ADD COLUMN closed_at     TIMESTAMPTZ,
    ADD CONSTRAINT auctions_anti_snipe_non_negative
        CHECK (extend_window >= interval '0' AND extend_by >= interval '0');

-- Closes from before this migration: the close event's time is the best
-- record of when they closed. The old trigger allows this update (status
-- and head are unchanged).
UPDATE auctions a
SET closed_at = GREATEST(a.end_at, COALESCE(
    (SELECT o.created_at FROM outbox o WHERE o.auction_id = a.id AND o.event_type = 'auction_closed'),
    a.end_at))
WHERE a.status = 'closed';

ALTER TABLE auctions ADD CONSTRAINT auctions_closed_at_iff_closed
    CHECK ((status = 'closed') = (closed_at IS NOT NULL));

-- The closer's scan: open auctions in end order.
CREATE INDEX auctions_open_by_end ON auctions (end_at) WHERE status = 'open';

-- +goose StatementBegin
CREATE FUNCTION auctions_insert_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    -- 'closed', not <> 'open': other values are the status CHECK's to report.
    IF NEW.status = 'closed' OR NEW.closed_at IS NOT NULL
       OR NEW.current_bid_id IS NOT NULL OR NEW.version <> 0 THEN
        RAISE EXCEPTION 'a new auction must be open, without bids, at version 0'
            USING ERRCODE = 'AE017';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER auctions_insert_guard BEFORE INSERT ON auctions
    FOR EACH ROW EXECUTE FUNCTION auctions_insert_guard();

-- The M4 guard (00013) plus AE018, AE019, the version bump and closed_at.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION auctions_update_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    bid_at   TIMESTAMPTZ;
    expected TIMESTAMPTZ := OLD.end_at;
BEGIN
    IF NEW.status IS DISTINCT FROM OLD.status
       AND NOT (OLD.status = 'open' AND NEW.status = 'closed') THEN
        RAISE EXCEPTION 'auction % cannot change status from % to %', OLD.id, OLD.status, NEW.status
            USING ERRCODE = 'AE010';
    END IF;

    IF OLD.status = 'closed' THEN
        RAISE EXCEPTION 'auction % is closed and cannot change', OLD.id USING ERRCODE = 'AE019';
    END IF;
    IF (NEW.id, NEW.item_id, NEW.start_at, NEW.starting_price, NEW.min_increment,
        NEW.extend_window, NEW.extend_by, NEW.created_at)
       IS DISTINCT FROM
       (OLD.id, OLD.item_id, OLD.start_at, OLD.starting_price, OLD.min_increment,
        OLD.extend_window, OLD.extend_by, OLD.created_at) THEN
        RAISE EXCEPTION 'auction % terms cannot change', OLD.id USING ERRCODE = 'AE019';
    END IF;

    IF NEW.current_bid_id IS DISTINCT FROM OLD.current_bid_id THEN
        SELECT created_at INTO bid_at FROM bids
        WHERE id = NEW.current_bid_id
          AND auction_id = NEW.id
          AND prev_bid_id IS NOT DISTINCT FROM OLD.current_bid_id;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'auction % head may only advance to a bid that extends bid %', OLD.id, OLD.current_bid_id
                USING ERRCODE = 'AE011';
        END IF;
        -- Anti-snipe. With extend_window = 0 this never fires, because the
        -- bid guard only accepts bids before end_at.
        IF bid_at >= OLD.end_at - OLD.extend_window THEN
            expected := GREATEST(OLD.end_at, bid_at + OLD.extend_by);
        END IF;
    END IF;
    IF NEW.end_at IS DISTINCT FROM expected THEN
        RAISE EXCEPTION 'auction % end may only move by the anti-snipe rule (expected %, got %)', OLD.id, expected, NEW.end_at
            USING ERRCODE = 'AE018';
    END IF;

    IF OLD.status = 'open' AND NEW.status = 'closed' THEN
        NEW.closed_at := clock_timestamp();
        IF NEW.closed_at < NEW.end_at THEN
            RAISE EXCEPTION 'auction % cannot close before its end (%)', OLD.id, NEW.end_at
                USING ERRCODE = 'AE012';
        END IF;
    ELSE
        NEW.closed_at := OLD.closed_at;
    END IF;

    NEW.version := OLD.version + 1;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER auctions_insert_guard ON auctions;
DROP FUNCTION auctions_insert_guard();

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

DROP INDEX auctions_open_by_end;
ALTER TABLE auctions
    DROP CONSTRAINT auctions_closed_at_iff_closed,
    DROP CONSTRAINT auctions_anti_snipe_non_negative,
    DROP COLUMN closed_at,
    DROP COLUMN extend_by,
    DROP COLUMN extend_window,
    DROP COLUMN version;
