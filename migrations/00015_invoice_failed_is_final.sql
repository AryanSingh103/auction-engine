-- +goose Up
-- AE016 no longer allows failed -> pending. It was meant for replaying a
-- dead-lettered settlement, but those invoices stay pending (a charge may
-- exist), and a failed invoice was declined, so no code path needs it; it
-- would only let a declined invoice later become paid. Now failed is as
-- final as paid. Found by the M4 adversarial review.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION invoices_guard() RETURNS trigger
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
       AND (OLD.status, NEW.status) NOT IN (('pending', 'paid'), ('pending', 'failed')) THEN
        RAISE EXCEPTION 'invoice % cannot move from % to %', OLD.id, OLD.status, NEW.status USING ERRCODE = 'AE016';
    END IF;
    IF NEW.status IS NOT DISTINCT FROM OLD.status THEN
        RAISE EXCEPTION 'invoice % can only change by moving status', OLD.id USING ERRCODE = 'AE016';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION invoices_guard() RETURNS trigger
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
