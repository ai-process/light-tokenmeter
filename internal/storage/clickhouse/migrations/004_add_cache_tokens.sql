ALTER TABLE usage_events ADD COLUMN IF NOT EXISTS cache_read_tokens Int64 DEFAULT 0, ADD COLUMN IF NOT EXISTS cache_write_tokens Int64 DEFAULT 0
