-- +goose Up
-- +goose StatementBegin
-- Parts of tokens_input, kept apart because vendors bill cached input at a
-- fraction of the input rate (and cache writes above it).
ALTER TABLE usage_events
    ADD COLUMN IF NOT EXISTS cache_read_tokens BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS cache_write_tokens BIGINT NOT NULL DEFAULT 0;
COMMENT ON COLUMN usage_events.cache_read_tokens IS 'Input tokens read from the prompt cache (part of tokens_input)';
COMMENT ON COLUMN usage_events.cache_write_tokens IS 'Input tokens written to the prompt cache (part of tokens_input)';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE usage_events DROP COLUMN IF EXISTS cache_read_tokens, DROP COLUMN IF EXISTS cache_write_tokens;
-- +goose StatementEnd
