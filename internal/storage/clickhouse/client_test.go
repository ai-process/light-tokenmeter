package clickhouse

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ai-process/light-tokenmeter/internal/domain"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getTestClient(t *testing.T) *Client {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err := Connect(ctx, Config{
		Addr:     envOr("CLICKHOUSE_TEST_ADDR", "localhost:9000"),
		Database: "tokenmeter_test",
		User:     envOr("CLICKHOUSE_TEST_USER", "tokenmeter"),
		Password: envOr("CLICKHOUSE_TEST_PASSWORD", "tokenmeter"),
	})
	if err != nil {
		t.Skipf("cannot connect to test clickhouse: %v", err)
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Errorf("close clickhouse: %v", err)
		}
	})

	// Start from scratch so every migration, including ALTERs, is exercised.
	require.NoError(t, client.conn.Exec(ctx, "DROP TABLE IF EXISTS usage_events"))
	require.NoError(t, client.conn.Exec(ctx, "DROP TABLE IF EXISTS schema_migrations"))
	require.NoError(t, client.EnsureSchema(ctx))
	return client
}

func TestClickHouse_CostAggregation(t *testing.T) {
	client := getTestClient(t)
	ctx := context.Background()

	writer := NewBatchWriter(client.Conn(), 100, time.Hour)
	writer.Start()

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
	writer.Enqueue(
		newEvent("gpt-4o", 1_000_000),
		newEvent("gpt-4o", 2_000_000),
		newEvent("gpt-4o-mini", 150_000),
	)
	require.NoError(t, writer.Close())

	now := time.Now().UTC()
	summary, _, err := client.QuerySummary(ctx, domain.SummaryParams{
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

	metaRows, _, err := client.QueryUsageByMeta(ctx, domain.MetaUsageParams{MetaKey: "job_id", MetaValue: "job-cost"})
	require.NoError(t, err)
	var total int64
	for _, r := range metaRows {
		total += r.CostNanoUSD
	}
	assert.Equal(t, int64(3_150_000), total)
}
