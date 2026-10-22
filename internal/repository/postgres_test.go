package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ai-process/light-tokenmeter/internal/domain"
	"github.com/ai-process/light-tokenmeter/internal/testdb"
)

func getTestPool(t *testing.T) (*pgxpool.Pool, *Repository) {
	t.Helper()
	pool := testdb.Postgres(t)
	return pool, New(pool)
}

func TestExplainJobIdIndex(t *testing.T) {
	pool, repo := getTestPool(t)
	ctx := context.Background()

	events := make([]*domain.UsageEvent, 0, 500)
	for i := 0; i < 500; i++ {
		events = append(events, &domain.UsageEvent{
			EventID:      uuid.New(),
			UserID:       "user-1",
			ProjectID:    "proj-1",
			ActionID:     "act-1",
			Vendor:       "openai",
			Model:        "gpt-4o",
			Status:       domain.StatusSuccess,
			TokensInput:  10,
			TokensOutput: 20,
			TokensTotal:  30,
			Meta:         map[string]string{"job_id": fmt.Sprintf("job-%d", i%10)},
		})
	}
	_, err := repo.InsertBatch(ctx, events)
	require.NoError(t, err)

	_, err = pool.Exec(ctx, "VACUUM ANALYZE usage_events;")
	require.NoError(t, err)

	// Explain QueryUsageByMeta for job_id
	explainQuery := `EXPLAIN SELECT COALESCE(meta->>$1, '') AS group_value, vendor, model,
		SUM(tokens_input), SUM(tokens_output), SUM(tokens_total), COUNT(*)
		FROM usage_events
		WHERE meta ? 'job_id' AND meta->>'job_id' = $2
		GROUP BY group_value, vendor, model`

	rows, err := pool.Query(ctx, explainQuery, "", "job-1")
	require.NoError(t, err)
	defer rows.Close()

	var planLines []string
	for rows.Next() {
		var line string
		err := rows.Scan(&line)
		require.NoError(t, err)
		planLines = append(planLines, line)
	}

	plan := fmt.Sprint(planLines)
	assert.Contains(t, plan, "idx_usage_events_meta_job", "expected plan to use idx_usage_events_meta_job index")
}

func TestQuerySummary_OccurredAt(t *testing.T) {
	_, repo := getTestPool(t)
	ctx := context.Background()

	now := time.Now().UTC()
	occurred3DaysAgo := now.Add(-72 * time.Hour)

	// Insert event with past occurred_at
	_, err := repo.InsertOne(ctx, &domain.UsageEvent{
		EventID:      uuid.New(),
		OccurredAt:   &occurred3DaysAgo,
		UserID:       "backfill-user",
		ProjectID:    "backfill-proj",
		ActionID:     "act",
		Vendor:       "openai",
		Model:        "gpt-4o",
		Status:       domain.StatusSuccess,
		TokensInput:  100,
		TokensOutput: 200,
		TokensTotal:  300,
	})
	require.NoError(t, err)

	// Query summary covering occurred3DaysAgo
	summaryRows, truncated, err := repo.QuerySummary(ctx, domain.SummaryParams{
		From:             now.Add(-96 * time.Hour),
		To:               now.Add(-48 * time.Hour),
		GroupByProjectID: true,
	})
	require.NoError(t, err)
	assert.False(t, truncated)
	require.Len(t, summaryRows, 1)
	assert.Equal(t, int64(300), summaryRows[0].TokensTotal)

	// Query summary covering only today (should not find the backfilled event)
	todaySummary, _, err := repo.QuerySummary(ctx, domain.SummaryParams{
		From:             now.Add(-12 * time.Hour),
		To:               now.Add(12 * time.Hour),
		GroupByProjectID: true,
	})
	require.NoError(t, err)
	assert.Empty(t, todaySummary, "event with past occurred_at should not appear in today's summary")
}

func TestQuerySummary_TruncatedFlag(t *testing.T) {
	pool, repo := getTestPool(t)
	ctx := context.Background()

	now := time.Now().UTC()

	// Insert 10005 events with different action_ids so grouping produces 10005 rows
	// We can generate rows directly using generate_series for speed
	insertSQL := `
		INSERT INTO usage_events (
			event_id, created_at, occurred_at, user_id, project_id, action_id,
			vendor, model, status, tokens_input, tokens_output, tokens_total
		)
		SELECT
			gen_random_uuid(),
			$1,
			$1,
			'u1',
			'p1',
			'act-' || i::text,
			'openai',
			'gpt-4o',
			1,
			10,
			20,
			30
		FROM generate_series(1, 10005) AS s(i);
	`
	_, err := pool.Exec(ctx, insertSQL, now)
	require.NoError(t, err)

	rows, truncated, err := repo.QuerySummary(ctx, domain.SummaryParams{
		From:            now.Add(-1 * time.Hour),
		To:              now.Add(1 * time.Hour),
		GroupByActionID: true,
	})
	require.NoError(t, err)
	assert.True(t, truncated, "expected truncated to be true when exceeding 10000 rows")
	assert.Len(t, rows, 10000, "expected exactly 10000 rows when truncated")
}

func TestQueryUsageByMeta_TruncatedFlag(t *testing.T) {
	pool, repo := getTestPool(t)
	ctx := context.Background()

	// Insert 10005 events with same job_id but different group_by_meta_key values
	insertSQL := `
		INSERT INTO usage_events (
			event_id, user_id, project_id, action_id,
			vendor, model, status, tokens_input, tokens_output, tokens_total, meta
		)
		SELECT
			gen_random_uuid(),
			'u1',
			'p1',
			'act',
			'openai',
			'gpt-4o',
			1,
			10,
			20,
			30,
			jsonb_build_object('job_id', 'job-xyz', 'task_id', 'task-' || i::text)
		FROM generate_series(1, 10005) AS s(i);
	`
	_, err := pool.Exec(ctx, insertSQL)
	require.NoError(t, err)

	rows, truncated, err := repo.QueryUsageByMeta(ctx, domain.MetaUsageParams{
		MetaKey:        "job_id",
		MetaValue:      "job-xyz",
		GroupByMetaKey: "task_id",
	})
	require.NoError(t, err)
	assert.True(t, truncated, "expected truncated to be true when exceeding 10000 rows")
	assert.Len(t, rows, 10000, "expected exactly 10000 rows when truncated")
}

func TestCostAggregation(t *testing.T) {
	_, repo := getTestPool(t)
	ctx := context.Background()

	newEvent := func(model string, cost int64) *domain.UsageEvent {
		return &domain.UsageEvent{
			EventID:      uuid.New(),
			UserID:       "u1",
			ProjectID:    "p1",
			ActionID:     "a1",
			Vendor:       "openai",
			Model:        model,
			Status:       domain.StatusSuccess,
			TokensInput:  10,
			TokensOutput: 20,
			TokensTotal:  30,
			Meta:         map[string]string{"job_id": "job-cost"},
			CostNanoUSD:  cost,
		}
	}

	_, err := repo.InsertOne(ctx, newEvent("gpt-4o", 1_000_000))
	require.NoError(t, err)
	_, err = repo.InsertBatch(ctx, []*domain.UsageEvent{
		newEvent("gpt-4o", 2_000_000),
		newEvent("gpt-4o-mini", 150_000),
	})
	require.NoError(t, err)

	now := time.Now().UTC()
	summary, _, err := repo.QuerySummary(ctx, domain.SummaryParams{
		From:         now.Add(-time.Hour),
		To:           now.Add(time.Hour),
		GroupByModel: true,
	})
	require.NoError(t, err)
	byModel := map[string]int64{}
	for _, r := range summary {
		byModel[r.Model] += r.CostNanoUSD
	}
	assert.Equal(t, int64(3_000_000), byModel["gpt-4o"])
	assert.Equal(t, int64(150_000), byModel["gpt-4o-mini"])

	metaRows, _, err := repo.QueryUsageByMeta(ctx, domain.MetaUsageParams{MetaKey: "job_id", MetaValue: "job-cost"})
	require.NoError(t, err)
	var total int64
	for _, r := range metaRows {
		total += r.CostNanoUSD
	}
	assert.Equal(t, int64(3_150_000), total)
}

// A retried batch inserts nothing new, and InsertBatch says so by ID, so the
// caller can keep duplicates out of downstream sinks.
func TestInsertBatchReturnsOnlyNewIDs(t *testing.T) {
	_, repo := getTestPool(t)
	ctx := context.Background()

	now := time.Now()
	mk := func() *domain.UsageEvent {
		return &domain.UsageEvent{
			EventID: uuid.New(), OccurredAt: &now, UserID: "u", ProjectID: "p", ActionID: "a",
			Vendor: "openai", Model: "m", Status: domain.StatusSuccess, TokensInput: 1, TokensOutput: 1, TokensTotal: 2,
		}
	}
	first := []*domain.UsageEvent{mk(), mk()}
	created, err := repo.InsertBatch(ctx, first)
	require.NoError(t, err)
	require.Len(t, created, 2)

	retry := append(first, mk())
	created, err = repo.InsertBatch(ctx, retry)
	require.NoError(t, err)
	require.Len(t, created, 1)
	_, ok := created[retry[2].EventID]
	require.True(t, ok, "only the new event is reported as created")
}
