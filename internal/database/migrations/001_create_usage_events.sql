-- +goose Up
-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS usage_events (
    event_id UUID PRIMARY KEY,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    occurred_at TIMESTAMPTZ,
    user_id VARCHAR(256) NOT NULL,
    project_id VARCHAR(256) NOT NULL,
    action_id VARCHAR(256) NOT NULL,
    vendor VARCHAR(64) NOT NULL,
    model VARCHAR(128) NOT NULL,
    status SMALLINT NOT NULL,
    duration_ms BIGINT NOT NULL DEFAULT 0,
    tokens_input BIGINT NOT NULL DEFAULT 0,
    tokens_output BIGINT NOT NULL DEFAULT 0,
    tokens_total BIGINT NOT NULL DEFAULT 0,
    error_class VARCHAR(256),
    vendor_http_status INTEGER,
    meta JSONB
);

-- Index on created_at for time-range queries
CREATE INDEX IF NOT EXISTS idx_usage_events_created_at ON usage_events (created_at);

-- Index on project_id + created_at for project-scoped queries
CREATE INDEX IF NOT EXISTS idx_usage_events_project_created ON usage_events (project_id, created_at);

-- Index on user_id + created_at for user-scoped queries
CREATE INDEX IF NOT EXISTS idx_usage_events_user_created ON usage_events (user_id, created_at);

-- Add comment for documentation
COMMENT ON TABLE usage_events IS 'Append-only log of LLM usage events for token tracking and cost analysis';
COMMENT ON COLUMN usage_events.event_id IS 'UUID provided by caller for idempotency';
COMMENT ON COLUMN usage_events.status IS '1=success, 2=error, 3=cancelled, 4=timeout';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS usage_events;
-- +goose StatementEnd
