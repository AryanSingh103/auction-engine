-- +goose Up
-- Bids and outbox events are an append-only ledger:
--   AE008: a bid can never be updated, deleted or truncated. A changed
--          amount or time would silently falsify invariants 2-4.
--   AE009: an outbox event can never be deleted or truncated (invariant 6,
--          "no bid without its event", must hold forever, not only at
--          insert), and the only permitted update is marking it published
--          once (published_at NULL -> a timestamp), for the M4 publisher.
-- Found by the milestone 1 adversarial review.
-- +goose StatementBegin
CREATE FUNCTION bids_append_only() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'bids are append-only (% rejected)', TG_OP USING ERRCODE = 'AE008';
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION outbox_append_only() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'UPDATE'
       AND OLD.published_at IS NULL
       AND NEW.published_at IS NOT NULL
       AND (NEW.id, NEW.auction_id, NEW.event_type, NEW.payload, NEW.bid_id, NEW.created_at)
           IS NOT DISTINCT FROM
           (OLD.id, OLD.auction_id, OLD.event_type, OLD.payload, OLD.bid_id, OLD.created_at) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'outbox events are append-only; only marking one published is allowed (% rejected)', TG_OP
        USING ERRCODE = 'AE009';
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER bids_append_only BEFORE UPDATE OR DELETE ON bids
    FOR EACH ROW EXECUTE FUNCTION bids_append_only();
CREATE TRIGGER bids_no_truncate BEFORE TRUNCATE ON bids
    FOR EACH STATEMENT EXECUTE FUNCTION bids_append_only();
CREATE TRIGGER outbox_append_only BEFORE UPDATE OR DELETE ON outbox
    FOR EACH ROW EXECUTE FUNCTION outbox_append_only();
CREATE TRIGGER outbox_no_truncate BEFORE TRUNCATE ON outbox
    FOR EACH STATEMENT EXECUTE FUNCTION outbox_append_only();

-- +goose Down
DROP TRIGGER outbox_no_truncate ON outbox;
DROP TRIGGER outbox_append_only ON outbox;
DROP TRIGGER bids_no_truncate ON bids;
DROP TRIGGER bids_append_only ON bids;
DROP FUNCTION outbox_append_only();
DROP FUNCTION bids_append_only();
