-- +goose Up
-- Invariant 6, "no bid exists without its outbox event". A foreign key
-- cannot express this direction (the bid is inserted first), so a
-- constraint trigger checks it. DEFERRABLE INITIALLY DEFERRED makes it run
-- at COMMIT rather than right after the INSERT, giving the transaction room
-- to write the outbox row after the bid. A transaction that forgets the
-- event fails to commit with SQLSTATE AE007, and nothing is persisted.
-- +goose StatementBegin
CREATE FUNCTION bids_require_outbox() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM outbox WHERE bid_id = NEW.id) THEN
        RAISE EXCEPTION 'bid % has no outbox event', NEW.id USING ERRCODE = 'AE007';
    END IF;
    RETURN NULL; -- ignored for AFTER triggers
END;
$$;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER bids_require_outbox AFTER INSERT ON bids
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION bids_require_outbox();

-- +goose Down
DROP TRIGGER bids_require_outbox ON bids;
DROP FUNCTION bids_require_outbox();
