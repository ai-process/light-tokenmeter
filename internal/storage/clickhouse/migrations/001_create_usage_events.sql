CREATE TABLE IF NOT EXISTS usage_events (
    event_id UUID,
    created_at DateTime64(3),
    occurred_at Nullable(DateTime64(3)),
    timestamp DateTime64(3),
    user_id String,
    project_id String,
    action_id String,
    vendor LowCardinality(String),
    model LowCardinality(String),
    status Int16,
    duration_ms Int64,
    tokens_input Int64,
    tokens_output Int64,
    tokens_total Int64,
    error_class LowCardinality(String),
    vendor_http_status Nullable(Int32),
    meta Map(String, String)
) ENGINE = MergeTree()
ORDER BY (project_id, vendor, model, timestamp, event_id)
TTL toDateTime(timestamp) + INTERVAL 90 DAY;
