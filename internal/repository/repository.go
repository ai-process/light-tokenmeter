package repository

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog/log"

	"github.com/ai-process/light-tokenmeter/internal/domain"
)

// Repository handles database operations for usage events
type Repository struct {
	pool *pgxpool.Pool
}

// New creates a new repository with the given connection pool
func New(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

// InsertOne inserts a single usage event with idempotency (ON CONFLICT DO NOTHING)
func (r *Repository) InsertOne(ctx context.Context, event *domain.UsageEvent) (bool, error) {
	metaJSON, err := event.MetaJSON()
	if err != nil {
		return false, fmt.Errorf("marshal meta: %w", err)
	}

	query := `
		INSERT INTO usage_events (
			event_id, occurred_at, user_id, project_id, action_id,
			vendor, model, status, duration_ms,
			tokens_input, tokens_output, tokens_total,
			error_class, vendor_http_status, meta, cost_nanousd
		) VALUES (
			$1, $2, $3, $4, $5,
			$6, $7, $8, $9,
			$10, $11, $12,
			$13, $14, $15, $16
		)
		ON CONFLICT (event_id) DO NOTHING
	`

	tag, err := r.pool.Exec(ctx, query,
		event.EventID,
		event.OccurredAt,
		event.UserID,
		event.ProjectID,
		event.ActionID,
		event.Vendor,
		event.Model,
		int16(event.Status),
		event.DurationMs,
		event.TokensInput,
		event.TokensOutput,
		event.TokensTotal,
		nullableString(event.ErrorClass),
		nullableInt32(event.VendorHTTPStatus),
		metaJSON,
		event.CostNanoUSD,
	)
	if err != nil {
		return false, fmt.Errorf("insert event: %w", err)
	}

	return tag.RowsAffected() > 0, nil
}

// InsertBatch inserts multiple usage events efficiently using COPY protocol
// Returns the count of newly created events and duplicate events
// It returns the IDs actually inserted: duplicates of stored events are not, so
// downstream sinks fed from them see each event once, however often it is sent.
func (r *Repository) InsertBatch(ctx context.Context, events []*domain.UsageEvent) (created map[uuid.UUID]struct{}, err error) {
	if len(events) == 0 {
		return nil, nil
	}

	// Use a transaction with temporary table approach for idempotent batch insert
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback(ctx)
		}
	}()

	// Create temporary table
	_, err = tx.Exec(ctx, `
		CREATE TEMP TABLE temp_usage_events (LIKE usage_events INCLUDING DEFAULTS) ON COMMIT DROP
	`)
	if err != nil {
		return nil, fmt.Errorf("create temp table: %w", err)
	}

	// Prepare rows for COPY
	rows := make([][]interface{}, 0, len(events))
	for _, event := range events {
		metaJSON, err := event.MetaJSON()
		if err != nil {
			return nil, fmt.Errorf("marshal meta for event %s: %w", event.EventID, err)
		}

		rows = append(rows, []interface{}{
			event.EventID,
			event.OccurredAt,
			event.UserID,
			event.ProjectID,
			event.ActionID,
			event.Vendor,
			event.Model,
			int16(event.Status),
			event.DurationMs,
			event.TokensInput,
			event.TokensOutput,
			event.TokensTotal,
			nullableString(event.ErrorClass),
			nullableInt32(event.VendorHTTPStatus),
			metaJSON,
			event.CostNanoUSD,
		})
	}

	// COPY into temp table
	copyCount, err := tx.CopyFrom(
		ctx,
		pgx.Identifier{"temp_usage_events"},
		[]string{
			"event_id", "occurred_at", "user_id", "project_id", "action_id",
			"vendor", "model", "status", "duration_ms",
			"tokens_input", "tokens_output", "tokens_total",
			"error_class", "vendor_http_status", "meta", "cost_nanousd",
		},
		pgx.CopyFromRows(rows),
	)
	if err != nil {
		return nil, fmt.Errorf("copy to temp table: %w", err)
	}

	log.Debug().Int64("copied", copyCount).Msg("copied events to temp table")

	// Insert from temp to main table with ON CONFLICT DO NOTHING
	inserted, err := tx.Query(ctx, `
		INSERT INTO usage_events
		SELECT * FROM temp_usage_events
		ON CONFLICT (event_id) DO NOTHING
		RETURNING event_id
	`)
	if err != nil {
		return nil, fmt.Errorf("insert from temp: %w", err)
	}
	ids, err := pgx.CollectRows(inserted, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, fmt.Errorf("insert from temp: %w", err)
	}

	if err = tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	created = make(map[uuid.UUID]struct{}, len(ids))
	for _, id := range ids {
		created[id] = struct{}{}
	}
	return created, nil
}

// QuerySummary returns aggregated usage statistics
func (r *Repository) QuerySummary(ctx context.Context, params domain.SummaryParams) ([]domain.SummaryRow, bool, error) {
	// Build the query dynamically based on grouping options
	var selectCols []string
	var groupCols []string

	// Always group by day based on event time (occurred_at with created_at fallback)
	selectCols = append(selectCols, "date_trunc('day', COALESCE(occurred_at, created_at)) AS bucket")
	groupCols = append(groupCols, "bucket")

	// Add optional dimensions
	if params.GroupByProjectID {
		selectCols = append(selectCols, "project_id")
		groupCols = append(groupCols, "project_id")
	}
	if params.GroupByUserID {
		selectCols = append(selectCols, "user_id")
		groupCols = append(groupCols, "user_id")
	}
	if params.GroupByVendor {
		selectCols = append(selectCols, "vendor")
		groupCols = append(groupCols, "vendor")
	}
	if params.GroupByModel {
		selectCols = append(selectCols, "model")
		groupCols = append(groupCols, "model")
	}
	if params.GroupByActionID {
		selectCols = append(selectCols, "action_id")
		groupCols = append(groupCols, "action_id")
	}
	if params.GroupByStatus {
		selectCols = append(selectCols, "status")
		groupCols = append(groupCols, "status")
	}

	// Add aggregations
	selectCols = append(selectCols,
		"SUM(tokens_input) AS tokens_input",
		"SUM(tokens_output) AS tokens_output",
		"SUM(tokens_total) AS tokens_total",
		"COUNT(*) AS request_count",
		"SUM(duration_ms) AS sum_duration_ms",
		"SUM(cost_nanousd) AS cost_nanousd",
	)

	// Build WHERE clause
	var whereClauses []string
	var args []interface{}
	argNum := 1

	whereClauses = append(whereClauses, fmt.Sprintf("COALESCE(occurred_at, created_at) >= $%d", argNum))
	args = append(args, params.From)
	argNum++

	whereClauses = append(whereClauses, fmt.Sprintf("COALESCE(occurred_at, created_at) < $%d", argNum))
	args = append(args, params.To)
	argNum++

	if params.ProjectID != "" {
		whereClauses = append(whereClauses, fmt.Sprintf("project_id = $%d", argNum))
		args = append(args, params.ProjectID)
		argNum++
	}
	if params.UserID != "" {
		whereClauses = append(whereClauses, fmt.Sprintf("user_id = $%d", argNum))
		args = append(args, params.UserID)
		argNum++
	}
	if params.Vendor != "" {
		whereClauses = append(whereClauses, fmt.Sprintf("vendor = $%d", argNum))
		args = append(args, params.Vendor)
		argNum++
	}
	if params.Model != "" {
		whereClauses = append(whereClauses, fmt.Sprintf("model = $%d", argNum))
		args = append(args, params.Model)
		argNum++
	}
	if params.ActionID != "" {
		whereClauses = append(whereClauses, fmt.Sprintf("action_id = $%d", argNum))
		args = append(args, params.ActionID)
		argNum++
	}
	if params.Status != domain.StatusUnspecified {
		whereClauses = append(whereClauses, fmt.Sprintf("status = $%d", argNum))
		args = append(args, int16(params.Status))
		argNum++
	}

	query := fmt.Sprintf(`
		SELECT %s
		FROM usage_events
		WHERE %s
		GROUP BY %s
		ORDER BY bucket DESC
		LIMIT 10001
	`,
		strings.Join(selectCols, ", "),
		strings.Join(whereClauses, " AND "),
		strings.Join(groupCols, ", "),
	)

	log.Debug().Str("query", query).Interface("args", args).Msg("executing summary query")

	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, false, fmt.Errorf("query summary: %w", err)
	}
	defer rows.Close()

	var results []domain.SummaryRow
	for rows.Next() {
		var row domain.SummaryRow
		var status int16

		// Build scan destinations dynamically
		scanDest := []interface{}{&row.Bucket}
		if params.GroupByProjectID {
			scanDest = append(scanDest, &row.ProjectID)
		}
		if params.GroupByUserID {
			scanDest = append(scanDest, &row.UserID)
		}
		if params.GroupByVendor {
			scanDest = append(scanDest, &row.Vendor)
		}
		if params.GroupByModel {
			scanDest = append(scanDest, &row.Model)
		}
		if params.GroupByActionID {
			scanDest = append(scanDest, &row.ActionID)
		}
		if params.GroupByStatus {
			scanDest = append(scanDest, &status)
		}
		scanDest = append(scanDest,
			&row.TokensInput,
			&row.TokensOutput,
			&row.TokensTotal,
			&row.RequestCount,
			&row.SumDurationMs,
			&row.CostNanoUSD,
		)

		if err := rows.Scan(scanDest...); err != nil {
			return nil, false, fmt.Errorf("scan row: %w", err)
		}

		if params.GroupByStatus {
			row.Status = domain.Status(status)
		}

		results = append(results, row)
	}

	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("rows error: %w", err)
	}

	truncated := false
	if len(results) > 10000 {
		truncated = true
		results = results[:10000]
	}

	return results, truncated, nil
}

// QueryUsageByMeta aggregates usage over events carrying params.MetaKey =
// params.MetaValue, split by (group meta value, vendor, model).
func (r *Repository) QueryUsageByMeta(ctx context.Context, params domain.MetaUsageParams) ([]domain.MetaUsageRow, bool, error) {
	// Meta keys are JSONB path arguments, not identifiers.
	// When filtering on job_id, include the literal condition `meta ? 'job_id'`
	// and path expression so PostgreSQL can utilize the partial index
	// idx_usage_events_meta_job (ON ((meta->>'job_id')) WHERE meta ? 'job_id').
	var whereClause string
	var args []interface{}
	if params.MetaKey == "job_id" {
		whereClause = "WHERE meta ? 'job_id' AND meta->>'job_id' = $2"
		args = []interface{}{params.GroupByMetaKey, params.MetaValue}
	} else {
		whereClause = "WHERE meta->>$2 = $3"
		args = []interface{}{params.GroupByMetaKey, params.MetaKey, params.MetaValue}
	}

	if params.ProjectID != "" {
		whereClause += fmt.Sprintf(" AND project_id = $%d", len(args)+1)
		args = append(args, params.ProjectID)
	}

	query := fmt.Sprintf(`
		SELECT
			COALESCE(meta->>$1, '') AS group_value,
			vendor,
			model,
			SUM(tokens_input) AS tokens_input,
			SUM(tokens_output) AS tokens_output,
			SUM(tokens_total) AS tokens_total,
			COUNT(*) AS request_count,
			SUM(cost_nanousd) AS cost_nanousd
		FROM usage_events
		%s
		GROUP BY group_value, vendor, model
		ORDER BY group_value, vendor, model
		LIMIT 10001
	`, whereClause)

	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, false, fmt.Errorf("query usage by meta: %w", err)
	}
	defer rows.Close()

	var results []domain.MetaUsageRow
	for rows.Next() {
		var row domain.MetaUsageRow
		if err := rows.Scan(
			&row.GroupValue, &row.Vendor, &row.Model,
			&row.TokensInput, &row.TokensOutput, &row.TokensTotal, &row.RequestCount,
			&row.CostNanoUSD,
		); err != nil {
			return nil, false, fmt.Errorf("scan meta usage row: %w", err)
		}
		results = append(results, row)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("rows error: %w", err)
	}

	truncated := false
	if len(results) > 10000 {
		truncated = true
		results = results[:10000]
	}

	return results, truncated, nil
}

// Ping checks database connectivity
func (r *Repository) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return r.pool.Ping(ctx)
}

// Helper functions for nullable values
func nullableString(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

func nullableInt32(i int32) interface{} {
	if i == 0 {
		return nil
	}
	return i
}
