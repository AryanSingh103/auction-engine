-- +goose Up
-- One invoice per closed auction that had a winner, created by settlement
-- (M4) and charged through the payment provider with the invoice id as the
-- idempotency key (R3). Invariant 5, "exactly one charge per closed
-- auction", starts here: at most one invoice per auction, and it can only
-- say what the auction's result says.
--   AE015: an invoice must match its auction: closed, and bid, winner and
--          amount equal to the auction's final head.
--   AE016: status only moves pending -> paid, pending -> failed, or
--          failed -> pending (a replay after a dead-lettered settlement);
--          paid is final; nothing else about an invoice ever changes, and
--          invoices are never deleted.
CREATE TABLE invoices (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    auction_id BIGINT      NOT NULL UNIQUE REFERENCES auctions (id),
    bid_id     BIGINT      NOT NULL,
    winner_id  BIGINT      NOT NULL REFERENCES users (id),
    amount     BIGINT      NOT NULL CHECK (amount > 0),
    status     TEXT        NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'paid', 'failed')),
    -- The provider's charge id, set when paid.
    payment_id TEXT,
    -- Why settlement gave up, set when failed.
    failure    TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    settled_at TIMESTAMPTZ,

    CONSTRAINT invoices_bid_of_auction FOREIGN KEY (auction_id, bid_id) REFERENCES bids (auction_id, id),
    CONSTRAINT invoices_paid_has_payment CHECK ((status = 'paid') = (payment_id IS NOT NULL)),
    CONSTRAINT invoices_failed_has_reason CHECK ((status = 'failed') = (failure IS NOT NULL)),
    CONSTRAINT invoices_settled_when_not_pending CHECK ((status = 'pending') = (settled_at IS NULL))
);

-- +goose StatementBegin
CREATE FUNCTION invoices_match_auction() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM auctions
        WHERE id = NEW.auction_id
          AND status = 'closed'
          AND current_bid_id = NEW.bid_id
          AND current_leader_id = NEW.winner_id
          AND current_price = NEW.amount
    ) THEN
        RAISE EXCEPTION 'invoice for auction % does not match its closed result', NEW.auction_id
            USING ERRCODE = 'AE015';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER invoices_match_auction BEFORE INSERT ON invoices
    FOR EACH ROW EXECUTE FUNCTION invoices_match_auction();

-- +goose StatementBegin
CREATE FUNCTION invoices_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    -- Checked first and on its own: OLD is not assigned for TRUNCATE (a
    -- statement trigger), and PL/pgSQL does not promise to short-circuit.
    IF TG_OP <> 'UPDATE' THEN
        RAISE EXCEPTION 'invoices cannot be deleted (% rejected)', TG_OP USING ERRCODE = 'AE016';
    END IF;
    IF (NEW.id, NEW.auction_id, NEW.bid_id, NEW.winner_id, NEW.amount, NEW.created_at)
           IS DISTINCT FROM
           (OLD.id, OLD.auction_id, OLD.bid_id, OLD.winner_id, OLD.amount, OLD.created_at) THEN
        RAISE EXCEPTION 'invoice % identity cannot change', OLD.id USING ERRCODE = 'AE016';
    END IF;
    IF NEW.status IS DISTINCT FROM OLD.status
       AND (OLD.status, NEW.status) NOT IN (('pending', 'paid'), ('pending', 'failed'), ('failed', 'pending')) THEN
        RAISE EXCEPTION 'invoice % cannot move from % to %', OLD.id, OLD.status, NEW.status USING ERRCODE = 'AE016';
    END IF;
    IF NEW.status IS NOT DISTINCT FROM OLD.status THEN
        RAISE EXCEPTION 'invoice % can only change by moving status', OLD.id USING ERRCODE = 'AE016';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER invoices_guard BEFORE UPDATE OR DELETE ON invoices
    FOR EACH ROW EXECUTE FUNCTION invoices_guard();
CREATE TRIGGER invoices_no_truncate BEFORE TRUNCATE ON invoices
    FOR EACH STATEMENT EXECUTE FUNCTION invoices_guard();

-- +goose Down
DROP TABLE invoices;
DROP FUNCTION invoices_guard();
DROP FUNCTION invoices_match_auction();
