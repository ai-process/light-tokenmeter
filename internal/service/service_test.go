package service

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ai-process/light-tokenmeter/internal/domain"
	"github.com/ai-process/light-tokenmeter/internal/metrics"
	"github.com/ai-process/light-tokenmeter/internal/repository"
	"github.com/ai-process/light-tokenmeter/internal/testdb"
)

type mockAnalytics struct {
	summaryCalled bool
	metaCalled    bool
	pingCalled    bool
}

func (m *mockAnalytics) QuerySummary(ctx context.Context, params domain.SummaryParams) ([]domain.SummaryRow, bool, error) {
	m.summaryCalled = true
	return []domain.SummaryRow{
		{
			Bucket:       time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC),
			ProjectID:    params.ProjectID,
			TokensTotal:  1234,
			RequestCount: 10,
		},
	}, false, nil
}

func (m *mockAnalytics) QueryUsageByMeta(ctx context.Context, params domain.MetaUsageParams) ([]domain.MetaUsageRow, bool, error) {
	m.metaCalled = true
	return []domain.MetaUsageRow{
		{
			GroupValue:   "task-1",
			Vendor:       "openai",
			Model:        "gpt-4o",
			TokensTotal:  500,
			RequestCount: 5,
		},
	}, false, nil
}

func (m *mockAnalytics) Ping(ctx context.Context) error {
	m.pingCalled = true
	return nil
}

func TestUsageService_AnalyticsDelegation(t *testing.T) {
	analytics := &mockAnalytics{}
	svc := NewUsageService(nil).WithAnalytics(analytics)

	rows, truncated, err := svc.GetSummary(context.Background(), domain.SummaryParams{
		ProjectID: "test-proj",
		From:      time.Now().Add(-24 * time.Hour),
		To:        time.Now(),
	})
	require.NoError(t, err)
	assert.False(t, truncated)
	assert.True(t, analytics.summaryCalled)
	require.Len(t, rows, 1)
	assert.Equal(t, "test-proj", rows[0].ProjectID)
	assert.Equal(t, int64(1234), rows[0].TokensTotal)

	metaRows, truncated, err := svc.GetUsageByMeta(context.Background(), domain.MetaUsageParams{
		MetaKey:   "task_id",
		MetaValue: "task-1",
	})
	require.NoError(t, err)
	assert.False(t, truncated)
	assert.True(t, analytics.metaCalled)
	require.Len(t, metaRows, 1)
	assert.Equal(t, "task-1", metaRows[0].GroupValue)
	assert.Equal(t, int64(500), metaRows[0].TokensTotal)
}

// A batch sent again (the client lost the reply and retried) must not reach
// the ClickHouse outbox twice: ClickHouse would count its tokens and cost twice.
func TestRecordEventsQueuesOnlyNewEventsForClickHouse(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testdb.Postgres(t)

	svc := NewUsageService(repository.New(pool).WithOutbox())
	created0 := testutil.ToFloat64(metrics.Events.WithLabelValues("created"))
	dups0 := testutil.ToFloat64(metrics.Events.WithLabelValues("duplicate"))
	ev := &domain.UsageEvent{
		EventID: uuid.New(), OccurredAt: ptrTime(time.Now()), UserID: "u", ProjectID: "p", ActionID: "a",
		Vendor: "openai", Model: "m", Status: domain.StatusSuccess, TokensInput: 1, TokensOutput: 1, TokensTotal: 2,
	}
	created, dups, err := svc.RecordEvents(ctx, []*domain.UsageEvent{ev, ev})
	require.NoError(t, err)
	require.Equal(t, 1, created)
	require.Equal(t, 1, dups)
	_, _, err = svc.RecordEvents(ctx, []*domain.UsageEvent{ev})
	require.NoError(t, err)
	created1, err := svc.RecordEvent(ctx, ev)
	require.NoError(t, err)
	require.False(t, created1)

	var queued int
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM clickhouse_outbox").Scan(&queued))
	require.Equal(t, 1, queued, "the outbox must hold the event once")
	assert.Equal(t, created0+1, testutil.ToFloat64(metrics.Events.WithLabelValues("created")))
	assert.Equal(t, dups0+3, testutil.ToFloat64(metrics.Events.WithLabelValues("duplicate")))
}

func ptrTime(t time.Time) *time.Time { return &t }
