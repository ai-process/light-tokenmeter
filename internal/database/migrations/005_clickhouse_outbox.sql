-- +goose Up
-- +goose StatementBegin
-- Events waiting for ClickHouse, written in the same statement as the event so
-- a restart cannot lose one. batch_id doubles as ClickHouse's deduplication
-- token, so a batch retried after a crash is not counted twice.
CREATE TABLE IF NOT EXISTS clickhouse_outbox (
    event_id UUID PRIMARY KEY REFERENCES usage_events (event_id) ON DELETE CASCADE,
    enqueued_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    batch_id UUID,
    -- A claimed batch may be taken over once its lease ends.
    lease_until TIMESTAMPTZ,
    lease_owner UUID,
    -- ClickHouse refusals of this batch, which space its retries out.
    attempts INT NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_clickhouse_outbox_pending
    ON clickhouse_outbox (enqueued_at) WHERE batch_id IS NULL;
CREATE INDEX IF NOT EXISTS idx_clickhouse_outbox_lease
    ON clickhouse_outbox (lease_until) WHERE batch_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_clickhouse_outbox_batch
    ON clickhouse_outbox (batch_id) WHERE batch_id IS NOT NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS clickhouse_outbox;
-- +goose StatementEnd
