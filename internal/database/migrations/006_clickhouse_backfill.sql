-- +goose Up
-- +goose StatementBegin
-- One row once Postgres history has been queued for ClickHouse (or found
-- already there), so the backfill runs once per database.
CREATE TABLE IF NOT EXISTS clickhouse_backfill (
    done_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    queued BIGINT NOT NULL
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS clickhouse_backfill;
-- +goose StatementEnd
