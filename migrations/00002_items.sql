-- +goose Up
-- The thing being auctioned: a storage unit. photo_keys are object keys in
-- S3 (uploads are out of scope; the keys are stored only).
CREATE TABLE items (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    title       TEXT        NOT NULL CHECK (length(title) BETWEEN 1 AND 200),
    description TEXT        NOT NULL DEFAULT '',
    photo_keys  TEXT[]      NOT NULL DEFAULT '{}',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE items;
