package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ai-process/light-tokenmeter/internal/domain"
)

type mockSink struct {
	events []*domain.UsageEvent
}

func (m *mockSink) Enqueue(events ...*domain.UsageEvent) {
	m.events = append(m.events, events...)
}

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

func TestUsageService_EventSinkConfiguration(t *testing.T) {
	sink := &mockSink{}
	svc := NewUsageService(nil).WithEventSink(sink)

	assert.Equal(t, sink, svc.sink)
}
