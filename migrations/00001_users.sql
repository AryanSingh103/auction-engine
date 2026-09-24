-- +goose Up
-- Bidders. There is no authentication (docs/decisions/010): requests name
-- their user in the X-User-ID header, and the foreign keys from bids ensure
-- only existing users can bid.
CREATE TABLE users (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name       TEXT        NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE users;
