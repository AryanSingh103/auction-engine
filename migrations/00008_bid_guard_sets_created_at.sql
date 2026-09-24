-- +goose Up
-- bids.created_at is what the invariant checker uses to prove no bid was
-- accepted after close (invariant 4), so it must be the exact time the
-- guard judged, set by the database. Before this, the column DEFAULT was
-- evaluated before the guard's lock wait and an INSERT could supply any
-- value. Found by the milestone 1 adversarial review.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION bids_guard() RETURNS trigger
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

    -- The recorded acceptance time is the time these checks used, not a
    -- client-supplied value or a DEFAULT evaluated before the lock wait.
    NEW.created_at := ts;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION bids_guard() RETURNS trigger
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
