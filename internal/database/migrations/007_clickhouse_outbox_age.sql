-- +goose Up
-- +goose StatementBegin
-- The oldest waiting event, claimed or not, is read every 15 s for the
-- outbox-age metric; without this index that is a full scan.
CREATE INDEX IF NOT EXISTS idx_clickhouse_outbox_enqueued ON clickhouse_outbox (enqueued_at);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_clickhouse_outbox_enqueued;
-- +goose StatementEnd
