-- +goose Up
-- Backstop for the bid rules (invariants 3 and 4, no self-outbid). The Go
-- bid path validates first and returns friendly errors; this trigger makes
-- the rules hold even if application code is wrong or bypassed.
-- Errors use the custom SQLSTATE class AE so Go can map them exactly:
--   AE001 not open, AE002 not started, AE003 ended, AE004 stale head,
--   AE005 self-outbid, AE006 too low.
-- +goose StatementBegin
CREATE FUNCTION bids_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    a        auctions%ROWTYPE;
    ts       TIMESTAMPTZ;
    required BIGINT;
BEGIN
    -- Take (or re-take, if the caller already holds it) the auction row
    -- lock, so these checks and the insert are atomic against other bids
    -- and against the closer.
    SELECT * INTO a FROM auctions WHERE id = NEW.auction_id FOR UPDATE;
    IF NOT FOUND THEN
        RETURN NEW; -- the foreign key reports the missing auction
    END IF;

    -- Read the clock only once the lock is held. clock_timestamp() is the
    -- real current time; now() would be this transaction's start time,
    -- which may be long before a lock wait ended (open question R1).
    ts := clock_timestamp();

    IF a.status <> 'open' THEN
        RAISE EXCEPTION 'auction % is not open', a.id USING ERRCODE = 'AE001';
    END IF;
    IF ts < a.start_at THEN
        RAISE EXCEPTION 'auction % has not started', a.id USING ERRCODE = 'AE002';
    END IF;
    IF ts >= a.end_at THEN
        RAISE EXCEPTION 'auction % has ended', a.id USING ERRCODE = 'AE003';
    END IF;
    IF NEW.prev_bid_id IS DISTINCT FROM a.current_bid_id THEN
        RAISE EXCEPTION 'bid on auction % does not extend the current head', a.id
            USING ERRCODE = 'AE004';
    END IF;
    IF a.current_leader_id = NEW.user_id THEN
        RAISE EXCEPTION 'user % already leads auction %', NEW.user_id, a.id USING ERRCODE = 'AE005';
    END IF;

    required := COALESCE(a.current_price + a.min_increment, a.starting_price);
    IF NEW.amount < required THEN
        RAISE EXCEPTION 'bid % below minimum % on auction %', NEW.amount, required, a.id
            USING ERRCODE = 'AE006';
    END IF;

    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER bids_guard BEFORE INSERT ON bids
    FOR EACH ROW EXECUTE FUNCTION bids_guard();

-- +goose Down
DROP TRIGGER bids_guard ON bids;
DROP FUNCTION bids_guard();
