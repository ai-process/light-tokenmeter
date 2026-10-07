-- +goose Up
-- +goose StatementBegin
-- Expression index for GetUsageByMeta lookups scoped to a job: callers stamp
-- meta.job_id on every call of a job. Partial, so it stays tiny.
CREATE INDEX IF NOT EXISTS idx_usage_events_meta_job
    ON usage_events ((meta->>'job_id'))
    WHERE meta ? 'job_id';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_usage_events_meta_job;
-- +goose StatementEnd
