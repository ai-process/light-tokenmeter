package clickhouse

import (
	"context"
	"embed"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/rs/zerolog/log"

	"github.com/ai-process/light-tokenmeter/internal/domain"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

const maxSummaryRows = 10000

// Client manages ClickHouse connections and analytical queries.
type Client struct {
	conn driver.Conn
	cfg  Config
}

// Connect establishes a connection to ClickHouse, ensuring the database exists.
func Connect(ctx context.Context, cfg Config) (*Client, error) {
	// Connect to default DB first to ensure target database exists.
	initOpts := &clickhouse.Options{
		Addr: []string{cfg.Addr},
		Auth: clickhouse.Auth{
			Database: "default",
			Username: cfg.User,
			Password: cfg.Password,
		},
		DialTimeout: 5 * time.Second,
	}
	initConn, err := clickhouse.Open(initOpts)
	if err == nil {
		// A least-privilege user may lack CREATE DATABASE for a database that
		// already exists; only a database that is really missing is an error,
		// reported here rather than as a later "database does not exist".
		if pingErr := initConn.Ping(ctx); pingErr == nil {
			if err := initConn.Exec(ctx, fmt.Sprintf("CREATE DATABASE IF NOT EXISTS `%s`", cfg.Database)); err != nil {
				var exists uint64
				if qerr := initConn.QueryRow(ctx, "SELECT count() FROM system.databases WHERE name = ?", cfg.Database).Scan(&exists); qerr != nil || exists == 0 {
					_ = initConn.Close()
					return nil, fmt.Errorf("create clickhouse database %s: %w", cfg.Database, err)
				}
			}
		}
		_ = initConn.Close()
	}

	opts := &clickhouse.Options{
		Addr: []string{cfg.Addr},
		Auth: clickhouse.Auth{
			Database: cfg.Database,
			Username: cfg.User,
			Password: cfg.Password,
		},
		DialTimeout: 5 * time.Second,
		Compression: &clickhouse.Compression{
			Method: clickhouse.CompressionLZ4,
		},
		Settings: clickhouse.Settings{
			"max_execution_time": 60,
		},
	}

	conn, err := clickhouse.Open(opts)
	if err != nil {
		return nil, fmt.Errorf("open clickhouse: %w", err)
	}

	if err := conn.Ping(ctx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("ping clickhouse: %w", err)
	}

	return &Client{conn: conn, cfg: cfg}, nil
}

// EnsureSchema applies any pending embedded migrations in sequential order.
func (c *Client) EnsureSchema(ctx context.Context) error {
	err := c.conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version UInt32,
			applied_at DateTime DEFAULT now()
		) ENGINE = TinyLog
	`)
	if err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	rows, err := c.conn.Query(ctx, "SELECT version FROM schema_migrations")
	if err != nil {
		return fmt.Errorf("query schema_migrations: %w", err)
	}
	defer rows.Close()

	applied := make(map[uint32]bool)
	for rows.Next() {
		var v uint32
		if err := rows.Scan(&v); err != nil {
			return fmt.Errorf("scan migration version: %w", err)
		}
		applied[v] = true
	}

	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("read migrations dir: %w", err)
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		var version uint32
		_, err := fmt.Sscanf(entry.Name(), "%d_", &version)
		if err != nil {
			continue
		}
		if applied[version] {
			continue
		}

		content, err := migrationsFS.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return fmt.Errorf("read migration %s: %w", entry.Name(), err)
		}

		statements := strings.Split(string(content), ";")
		for _, stmt := range statements {
			stmt = strings.TrimSpace(stmt)
			if stmt == "" {
				continue
			}
			if err := c.conn.Exec(ctx, stmt); err != nil {
				return fmt.Errorf("apply migration %s: %w", entry.Name(), err)
			}
		}

		if err := c.conn.Exec(ctx, "INSERT INTO schema_migrations (version) VALUES (?)", version); err != nil {
			return fmt.Errorf("record migration %s: %w", entry.Name(), err)
		}
		log.Info().Uint32("version", version).Str("file", entry.Name()).Msg("applied clickhouse migration")
	}

	return nil
}

// QuerySummary executes multi-dimensional analytical aggregations on ClickHouse.
func (c *Client) QuerySummary(ctx context.Context, params domain.SummaryParams) ([]domain.SummaryRow, bool, error) {
	var selectCols []string
	var groupCols []string

	selectCols = append(selectCols, "toDateTime(toStartOfDay(timestamp)) AS bucket")
	groupCols = append(groupCols, "bucket")

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

	selectCols = append(selectCols,
		"sum(tokens_input) AS tokens_input",
		"sum(tokens_output) AS tokens_output",
		"sum(tokens_total) AS tokens_total",
		"count() AS request_count",
		"sum(duration_ms) AS sum_duration_ms",
		"sum(cost_nanousd) AS cost_nanousd",
	)

	var whereClauses []string
	var args []any

	whereClauses = append(whereClauses, "timestamp >= ?")
	args = append(args, params.From)

	whereClauses = append(whereClauses, "timestamp < ?")
	args = append(args, params.To)

	if params.ProjectID != "" {
		whereClauses = append(whereClauses, "project_id = ?")
		args = append(args, params.ProjectID)
	}
	if params.UserID != "" {
		whereClauses = append(whereClauses, "user_id = ?")
		args = append(args, params.UserID)
	}
	if params.Vendor != "" {
		whereClauses = append(whereClauses, "vendor = ?")
		args = append(args, params.Vendor)
	}
	if params.Model != "" {
		whereClauses = append(whereClauses, "model = ?")
		args = append(args, params.Model)
	}
	if params.ActionID != "" {
		whereClauses = append(whereClauses, "action_id = ?")
		args = append(args, params.ActionID)
	}
	if params.Status != domain.StatusUnspecified {
		whereClauses = append(whereClauses, "status = ?")
		args = append(args, int16(params.Status))
	}

	query := fmt.Sprintf(`
		SELECT %s
		FROM usage_events
		WHERE %s
		GROUP BY %s
		ORDER BY %s
		LIMIT %d
	`,
		strings.Join(selectCols, ", "),
		strings.Join(whereClauses, " AND "),
		strings.Join(groupCols, ", "),
		strings.Join(groupCols, ", "),
		maxSummaryRows+1,
	)

	rows, err := c.conn.Query(ctx, query, args...)
	if err != nil {
		return nil, false, fmt.Errorf("clickhouse query summary: %w", err)
	}
	defer rows.Close()

	var results []domain.SummaryRow
	for rows.Next() {
		var row domain.SummaryRow
		var statusVal int16
		var reqCount uint64

		scanTargets := []any{&row.Bucket}

		if params.GroupByProjectID {
			scanTargets = append(scanTargets, &row.ProjectID)
		}
		if params.GroupByUserID {
			scanTargets = append(scanTargets, &row.UserID)
		}
		if params.GroupByVendor {
			scanTargets = append(scanTargets, &row.Vendor)
		}
		if params.GroupByModel {
			scanTargets = append(scanTargets, &row.Model)
		}
		if params.GroupByActionID {
			scanTargets = append(scanTargets, &row.ActionID)
		}
		if params.GroupByStatus {
			scanTargets = append(scanTargets, &statusVal)
		}

		scanTargets = append(scanTargets,
			&row.TokensInput,
			&row.TokensOutput,
			&row.TokensTotal,
			&reqCount,
			&row.SumDurationMs,
			&row.CostNanoUSD,
		)

		if err := rows.Scan(scanTargets...); err != nil {
			return nil, false, fmt.Errorf("clickhouse scan summary row: %w", err)
		}

		if params.GroupByStatus {
			row.Status = domain.Status(statusVal)
		}
		row.RequestCount = int64(reqCount)
		results = append(results, row)
	}

	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("clickhouse summary rows error: %w", err)
	}

	truncated := false
	if len(results) > maxSummaryRows {
		truncated = true
		results = results[:maxSummaryRows]
	}

	return results, truncated, nil
}

// QueryUsageByMeta aggregates token usage by meta key/value filtering.
func (c *Client) QueryUsageByMeta(ctx context.Context, params domain.MetaUsageParams) ([]domain.MetaUsageRow, bool, error) {
	var whereClauses []string
	var args []any

	whereClauses = append(whereClauses, "meta[?] = ?")
	args = append(args, params.MetaKey, params.MetaValue)

	if params.ProjectID != "" {
		whereClauses = append(whereClauses, "project_id = ?")
		args = append(args, params.ProjectID)
	}

	groupExpr := "''"
	if params.GroupByMetaKey != "" {
		groupExpr = "meta[?]"
		args = append([]any{params.GroupByMetaKey}, args...)
	}

	query := fmt.Sprintf(`
		SELECT
			%s AS group_value,
			vendor,
			model,
			sum(tokens_input) AS tokens_input,
			sum(tokens_output) AS tokens_output,
			sum(tokens_total) AS tokens_total,
			count() AS request_count,
			sum(cost_nanousd) AS cost_nanousd
		FROM usage_events
		WHERE %s
		GROUP BY group_value, vendor, model
		ORDER BY group_value, vendor, model
		LIMIT %d
	`,
		groupExpr,
		strings.Join(whereClauses, " AND "),
		maxSummaryRows+1,
	)

	rows, err := c.conn.Query(ctx, query, args...)
	if err != nil {
		return nil, false, fmt.Errorf("clickhouse query usage by meta: %w", err)
	}
	defer rows.Close()

	var results []domain.MetaUsageRow
	for rows.Next() {
		var row domain.MetaUsageRow
		var reqCount uint64

		if err := rows.Scan(
			&row.GroupValue,
			&row.Vendor,
			&row.Model,
			&row.TokensInput,
			&row.TokensOutput,
			&row.TokensTotal,
			&reqCount,
			&row.CostNanoUSD,
		); err != nil {
			return nil, false, fmt.Errorf("clickhouse scan meta usage row: %w", err)
		}
		row.RequestCount = int64(reqCount)
		results = append(results, row)
	}

	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("clickhouse meta rows error: %w", err)
	}

	truncated := false
	if len(results) > maxSummaryRows {
		truncated = true
		results = results[:maxSummaryRows]
	}

	return results, truncated, nil
}

// Ping checks ClickHouse database connectivity.
func (c *Client) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return c.conn.Ping(ctx)
}

// Close closes the underlying ClickHouse connection.
func (c *Client) Close() error {
	return c.conn.Close()
}

// Empty reports whether usage_events holds no event yet.
func (c *Client) Empty(ctx context.Context) (bool, error) {
	var n uint64
	if err := c.conn.QueryRow(ctx, "SELECT count() FROM (SELECT 1 FROM usage_events LIMIT 1)").Scan(&n); err != nil {
		return false, fmt.Errorf("check clickhouse events: %w", err)
	}
	return n == 0, nil
}

// Conn returns the driver connection.
func (c *Client) Conn() driver.Conn {
	return c.conn
}
