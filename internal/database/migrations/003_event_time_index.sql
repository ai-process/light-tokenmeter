-- +goose Up
-- +goose StatementBegin
-- Expression indexes for queries filtered and bucketed by event time (occurred_at or created_at fallback).
CREATE INDEX IF NOT EXISTS idx_usage_events_event_time
    ON usage_events (COALESCE(occurred_at, created_at));

CREATE INDEX IF NOT EXISTS idx_usage_events_project_event_time
    ON usage_events (project_id, COALESCE(occurred_at, created_at));

CREATE INDEX IF NOT EXISTS idx_usage_events_user_event_time
    ON usage_events (user_id, COALESCE(occurred_at, created_at));
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_usage_events_user_event_time;
DROP INDEX IF EXISTS idx_usage_events_project_event_time;
DROP INDEX IF EXISTS idx_usage_events_event_time;
-- +goose StatementEnd
