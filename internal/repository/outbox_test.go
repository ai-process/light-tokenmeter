package repository

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ai-process/light-tokenmeter/internal/domain"
)

func outboxEvent(meta map[string]string) *domain.UsageEvent {
	now := time.Now().UTC().Truncate(time.Millisecond)
	return &domain.UsageEvent{
		EventID: uuid.New(), OccurredAt: &now, UserID: "u", ProjectID: "p", ActionID: "a",
		Vendor: "openai", Model: "m", Status: domain.StatusError, DurationMs: 12,
		TokensInput: 3, TokensOutput: 4, TokensTotal: 7, ErrorClass: "rate_limited", VendorHTTPStatus: 429,
		Meta: meta, CostNanoUSD: 99,
	}
}

func TestOutboxClaimAckAndReclaim(t *testing.T) {
	pool, repo := getTestPool(t)
	repo.WithOutbox()
	ctx := context.Background()

	first := outboxEvent(map[string]string{"job_id": "j"})
	_, err := repo.InsertOne(ctx, first)
	require.NoError(t, err)
	batch := make([]*domain.UsageEvent, 0, 4)
	for i := 0; i < 4; i++ {
		batch = append(batch, outboxEvent(nil))
	}
	_, err = repo.InsertBatch(ctx, batch)
	require.NoError(t, err)

	n, age, err := repo.OutboxBacklog(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 5, n)
	require.GreaterOrEqual(t, age, time.Duration(0))

	// Claims never hand out an event twice.
	b1, ev1, err := repo.ClaimOutbox(ctx, 3, time.Minute)
	require.NoError(t, err)
	b2, ev2, err := repo.ClaimOutbox(ctx, 3, time.Minute)
	require.NoError(t, err)
	require.NotEqual(t, b1, b2)
	require.Len(t, ev1, 3)
	require.Len(t, ev2, 2)
	b3, ev3, err := repo.ClaimOutbox(ctx, 3, time.Minute)
	require.NoError(t, err)
	require.Equal(t, uuid.Nil, b3, "nothing left to claim")
	require.Empty(t, ev3)

	// Events come back as they were stored.
	var stored *domain.UsageEvent
	for _, e := range append(ev1, ev2...) {
		if e.EventID == first.EventID {
			stored = e
		}
	}
	require.NotNil(t, stored)
	require.Equal(t, first.Meta, stored.Meta)
	require.Equal(t, first.OccurredAt.Unix(), stored.OccurredAt.Unix())
	require.Equal(t, first.Status, stored.Status)
	require.Equal(t, first.ErrorClass, stored.ErrorClass)
	require.Equal(t, first.VendorHTTPStatus, stored.VendorHTTPStatus)
	require.Equal(t, first.CostNanoUSD, stored.CostNanoUSD)
	require.False(t, stored.CreatedAt.IsZero())

	require.NoError(t, repo.AckOutbox(ctx, b1))

	// b2 is never acknowledged (its replica died): once stale it is handed
	// out again under the same ID, which ClickHouse uses to drop the repeat.
	_, err = pool.Exec(ctx, "UPDATE clickhouse_outbox SET claimed_at = now() - interval '10 minutes' WHERE batch_id = $1", b2)
	require.NoError(t, err)
	again, evAgain, err := repo.ClaimOutbox(ctx, 3, time.Minute)
	require.NoError(t, err)
	require.Equal(t, b2, again)
	require.ElementsMatch(t, ids(ev2), ids(evAgain))

	// A batch just reclaimed is fresh again: nobody else takes it over.
	_, err = pool.Exec(ctx, "UPDATE clickhouse_outbox SET claimed_at = now() - interval '10 minutes' WHERE batch_id = $1", b2)
	require.NoError(t, err)
	var wg sync.WaitGroup
	got := make([]uuid.UUID, 4)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b, _, err := repo.ClaimOutbox(ctx, 3, time.Minute)
			assert.NoError(t, err)
			got[i] = b
		}()
	}
	wg.Wait()
	takers := 0
	for _, b := range got {
		if b == b2 {
			takers++
		}
	}
	require.Equal(t, 1, takers, "concurrent reclaims must hand a stale batch to one replica: %v", got)

	require.NoError(t, repo.AckOutbox(ctx, b2))
	n, _, err = repo.OutboxBacklog(ctx)
	require.NoError(t, err)
	require.Zero(t, n)
}

func TestNoOutboxWithoutClickHouse(t *testing.T) {
	pool, repo := getTestPool(t)
	ctx := context.Background()
	_, err := repo.InsertOne(ctx, outboxEvent(nil))
	require.NoError(t, err)
	_, err = repo.InsertBatch(ctx, []*domain.UsageEvent{outboxEvent(nil)})
	require.NoError(t, err)
	var n int
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM clickhouse_outbox").Scan(&n))
	require.Zero(t, n)
}

func ids(events []*domain.UsageEvent) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(events))
	for _, e := range events {
		out = append(out, e.EventID)
	}
	return out
}
