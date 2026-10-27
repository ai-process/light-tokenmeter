-- +goose Up
-- +goose StatementBegin
-- Tool calls through an MCP gateway: which server and tool, and how large the
-- result was. Empty and 0 on LLM events.
ALTER TABLE usage_events
    ADD COLUMN IF NOT EXISTS tool_server TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS tool_name TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS response_bytes BIGINT NOT NULL DEFAULT 0;
COMMENT ON COLUMN usage_events.tool_server IS 'MCP server of a tool call, empty for an LLM call';
COMMENT ON COLUMN usage_events.tool_name IS 'Tool of a tool call, empty for an LLM call';
COMMENT ON COLUMN usage_events.response_bytes IS 'Size of the tool result in bytes';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE usage_events DROP COLUMN IF EXISTS tool_server, DROP COLUMN IF EXISTS tool_name, DROP COLUMN IF EXISTS response_bytes;
-- +goose StatementEnd
