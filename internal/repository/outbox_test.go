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

func fill(t *testing.T, repo *Repository, n int) []*domain.UsageEvent {
	t.Helper()
	events := make([]*domain.UsageEvent, 0, n)
	for i := 0; i < n; i++ {
		events = append(events, outboxEvent(nil))
	}
	_, err := repo.InsertBatch(context.Background(), events)
	require.NoError(t, err)
	return events
}

var owner = uuid.New()

func expire(t *testing.T, repo *Repository, batch uuid.UUID) {
	t.Helper()
	_, err := repo.pool.Exec(context.Background(),
		"UPDATE clickhouse_outbox SET lease_until = now() - interval '1 second' WHERE batch_id = $1", batch)
	require.NoError(t, err)
}

func backlog(t *testing.T, repo *Repository) (waiting, setAside int64) {
	t.Helper()
	waiting, _, setAside, err := repo.OutboxBacklog(context.Background())
	require.NoError(t, err)
	return waiting, setAside
}

func TestOutboxClaimAckAndTakeOver(t *testing.T) {
	_, repo := getTestPool(t)
	repo.WithOutbox()
	ctx := context.Background()

	first := outboxEvent(map[string]string{"job_id": "j"})
	_, err := repo.InsertOne(ctx, first)
	require.NoError(t, err)
	fill(t, repo, 4)
	waiting, _ := backlog(t, repo)
	require.EqualValues(t, 5, waiting)

	// Claims never hand out an event twice.
	b1, ev1, err := repo.ClaimOutbox(ctx, owner, 3, time.Minute)
	require.NoError(t, err)
	b2, ev2, err := repo.ClaimOutbox(ctx, owner, 3, time.Minute)
	require.NoError(t, err)
	require.NotEqual(t, b1.ID, b2.ID)
	require.Len(t, ev1, 3)
	require.Len(t, ev2, 2)
	b3, _, err := repo.ClaimOutbox(ctx, owner, 3, time.Minute)
	require.NoError(t, err)
	require.Equal(t, uuid.Nil, b3.ID, "nothing left to claim")

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

	require.NoError(t, repo.AckOutbox(ctx, b1.ID))

	// b2's replica died: once its lease ends it is handed out again under the
	// same ID, which ClickHouse uses to drop the repeat.
	expire(t, repo, b2.ID)
	again, evAgain, err := repo.ClaimOutbox(ctx, owner, 3, time.Minute)
	require.NoError(t, err)
	require.Equal(t, b2.ID, again.ID)
	require.ElementsMatch(t, ids(ev2), ids(evAgain))

	// A renewed lease keeps it from being taken over, and only its owner may renew it.
	ok, err := repo.RenewOutbox(ctx, owner, b2.ID, time.Minute)
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = repo.RenewOutbox(ctx, uuid.New(), b2.ID, time.Minute)
	require.NoError(t, err)
	require.False(t, ok, "a replica whose batch was taken over must let it go")
	b4, _, err := repo.ClaimOutbox(ctx, owner, 3, time.Minute)
	require.NoError(t, err)
	require.Equal(t, uuid.Nil, b4.ID)

	require.NoError(t, repo.AckOutbox(ctx, b2.ID))
	ok, err = repo.RenewOutbox(ctx, owner, b2.ID, time.Minute)
	require.NoError(t, err)
	require.False(t, ok, "an acknowledged batch cannot be renewed")
	waiting, _ = backlog(t, repo)
	require.Zero(t, waiting)
}

// Replicas racing for one expired batch: exactly one gets it.
func TestOutboxTakeOverHasOneWinner(t *testing.T) {
	_, repo := getTestPool(t)
	repo.WithOutbox()
	ctx := context.Background()
	fill(t, repo, 5)
	b, _, err := repo.ClaimOutbox(ctx, owner, 5, time.Minute)
	require.NoError(t, err)

	for round := 0; round < 10; round++ {
		expire(t, repo, b.ID)
		start := make(chan struct{})
		got := make([]uuid.UUID, 8)
		var wg sync.WaitGroup
		for i := range got {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				c, _, err := repo.ClaimOutbox(ctx, owner, 5, time.Minute)
				assert.NoError(t, err)
				got[i] = c.ID
			}()
		}
		close(start)
		wg.Wait()
		takers := 0
		for _, id := range got {
			if id == b.ID {
				takers++
			}
		}
		require.Equal(t, 1, takers, "round %d: %v", round, got)
	}
}

func TestOutboxRefusalsPostponeSplitAndSetAside(t *testing.T) {
	_, repo := getTestPool(t)
	repo.WithOutbox()
	ctx := context.Background()
	fill(t, repo, 5)

	b, _, err := repo.ClaimOutbox(ctx, owner, 5, time.Minute)
	require.NoError(t, err)
	require.Zero(t, b.Attempts)

	// Postponed: not claimable until the delay passes, then back with a count.
	require.NoError(t, repo.RetryOutboxLater(ctx, b.ID, time.Hour))
	none, _, err := repo.ClaimOutbox(ctx, owner, 5, time.Minute)
	require.NoError(t, err)
	require.Equal(t, uuid.Nil, none.ID)
	expire(t, repo, b.ID)
	again, _, err := repo.ClaimOutbox(ctx, owner, 5, time.Minute)
	require.NoError(t, err)
	require.Equal(t, b.ID, again.ID)
	require.Equal(t, 1, again.Attempts)

	// Split: two fresh batches covering the same events, claimable at once.
	require.NoError(t, repo.SplitOutbox(ctx, b.ID, 2))
	h1, e1, err := repo.ClaimOutbox(ctx, owner, 5, time.Minute)
	require.NoError(t, err)
	h2, e2, err := repo.ClaimOutbox(ctx, owner, 5, time.Minute)
	require.NoError(t, err)
	require.NotEqual(t, b.ID, h1.ID)
	require.NotEqual(t, b.ID, h2.ID)
	require.NotEqual(t, h1.ID, h2.ID)
	require.Equal(t, 2, h1.Attempts, "halves keep the count, so they split again at the next refusal")
	require.Len(t, append(e1, e2...), 5)
	require.ElementsMatch(t, []int{2, 3}, []int{len(e1), len(e2)})

	// Set aside: out of every batch, counted apart.
	require.NoError(t, repo.SetAsideOutbox(ctx, e1[0].EventID, "code: 53"))
	waiting, setAside := backlog(t, repo)
	require.EqualValues(t, 4, waiting)
	require.EqualValues(t, 1, setAside)
	require.NoError(t, repo.AckOutbox(ctx, h1.ID))
	require.NoError(t, repo.AckOutbox(ctx, h2.ID))
	waiting, setAside = backlog(t, repo)
	require.Zero(t, waiting)
	require.EqualValues(t, 1, setAside, "acknowledging a batch keeps what was set aside")
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
