-- +goose Up
-- The outbox relay (M4) repeatedly asks for the oldest unpublished events:
--   SELECT ... FROM outbox WHERE published_at IS NULL ORDER BY id LIMIT n
-- Rows are never deleted (AE009), so without this index every poll scans
-- the whole history. The partial index holds only the backlog, which stays
-- small while the relay keeps up, and is already in id order.
CREATE INDEX outbox_unpublished ON outbox (id) WHERE published_at IS NULL;

-- +goose Down
DROP INDEX outbox_unpublished;
