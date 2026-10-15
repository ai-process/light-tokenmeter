-- +goose Up
-- +goose StatementBegin
-- Cost is priced by the caller at settle time and kept as an immutable fact,
-- so later price-catalog changes never rewrite historical spend.
ALTER TABLE usage_events ADD COLUMN IF NOT EXISTS cost_nanousd BIGINT NOT NULL DEFAULT 0;
COMMENT ON COLUMN usage_events.cost_nanousd IS 'Call cost in nano-USD (1e-9 USD)';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE usage_events DROP COLUMN IF EXISTS cost_nanousd;
-- +goose StatementEnd
