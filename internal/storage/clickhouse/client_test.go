package clickhouse

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ai-process/light-tokenmeter/internal/domain"
	"github.com/ai-process/light-tokenmeter/internal/testdb"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

const testPrefix = "tokenmeter_test_"

var sweepOnce sync.Once

// getTestClient gives each test a database of its own. With REQUIRE_DB set
// (CI), no ClickHouse is a failure instead of a skip.
func getTestClient(t *testing.T) *Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	name := fmt.Sprintf("%s%d_%s", testPrefix, time.Now().Unix(), uuid.NewString()[:8])
	client, err := Connect(ctx, Config{
		Addr:     envOr("CLICKHOUSE_TEST_ADDR", "localhost:9000"),
		Database: name,
		User:     envOr("CLICKHOUSE_TEST_USER", "tokenmeter"),
		Password: envOr("CLICKHOUSE_TEST_PASSWORD", "tokenmeter"),
	})
	if err != nil {
		testdb.Unavailable(t, "cannot use test clickhouse: %v", err)
		return nil
	}
	t.Cleanup(func() {
		if err := client.conn.Exec(context.Background(), "DROP DATABASE IF EXISTS `"+name+"`"); err != nil {
			t.Logf("drop test database %s: %v", name, err)
		}
		if err := client.Close(); err != nil {
			t.Errorf("close clickhouse: %v", err)
		}
	})
	sweepOnce.Do(func() { sweepStale(ctx, t, client) })
	require.NoError(t, client.EnsureSchema(ctx))
	return client
}

// sweepStale drops test databases an interrupted run left behind.
func sweepStale(ctx context.Context, t *testing.T, c *Client) {
	rows, err := c.conn.Query(ctx, "SELECT name FROM system.databases WHERE startsWith(name, ?)", testPrefix)
	if err != nil {
		return
	}
	var stale []string
	for rows.Next() {
		var name string
		if rows.Scan(&name) != nil {
			continue
		}
		ts, _, _ := strings.Cut(strings.TrimPrefix(name, testPrefix), "_")
		if sec, err := strconv.ParseInt(ts, 10, 64); err == nil && time.Since(time.Unix(sec, 0)) > time.Hour {
			stale = append(stale, name)
		}
	}
	_ = rows.Close()
	for _, name := range stale {
		if err := c.conn.Exec(ctx, "DROP DATABASE IF EXISTS `"+name+"`"); err != nil {
			t.Logf("drop stale test database %s: %v", name, err)
		}
	}
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
