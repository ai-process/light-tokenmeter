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
		Meta: meta, CostNanoUSD: 99, CacheReadTokens: 2, CacheWriteTokens: 1,
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

func backlog(t *testing.T, repo *Repository) int64 {
	t.Helper()
	waiting, _, err := repo.OutboxBacklog(context.Background())
	require.NoError(t, err)
	return waiting
}

func TestOutboxClaimAckAndTakeOver(t *testing.T) {
	_, repo := getTestPool(t)
	repo.WithOutbox()
	ctx := context.Background()

	first := outboxEvent(map[string]string{"job_id": "j"})
	_, err := repo.InsertOne(ctx, first)
	require.NoError(t, err)
	fill(t, repo, 4)
	require.EqualValues(t, 5, backlog(t, repo))

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
	require.Equal(t, first.CacheReadTokens, stored.CacheReadTokens)
	require.Equal(t, first.CacheWriteTokens, stored.CacheWriteTokens)
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
	require.Zero(t, backlog(t, repo))
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

func TestOutboxPostponeKeepsTheBatch(t *testing.T) {
	_, repo := getTestPool(t)
	repo.WithOutbox()
	ctx := context.Background()
	fill(t, repo, 5)

	b, events, err := repo.ClaimOutbox(ctx, owner, 5, time.Minute)
	require.NoError(t, err)
	require.Zero(t, b.Attempts)

	ok, err := repo.RetryOutboxLater(ctx, uuid.New(), b.ID, time.Hour)
	require.NoError(t, err)
	require.False(t, ok, "only the lease owner may postpone")

	// Postponed: not claimable until the delay passes, then back whole, with
	// the same ID and a count.
	ok, err = repo.RetryOutboxLater(ctx, owner, b.ID, time.Hour)
	require.NoError(t, err)
	require.True(t, ok)
	none, _, err := repo.ClaimOutbox(ctx, owner, 5, time.Minute)
	require.NoError(t, err)
	require.Equal(t, uuid.Nil, none.ID)
	expire(t, repo, b.ID)
	again, evAgain, err := repo.ClaimOutbox(ctx, owner, 5, time.Minute)
	require.NoError(t, err)
	require.Equal(t, b.ID, again.ID)
	require.Equal(t, 1, again.Attempts)
	require.ElementsMatch(t, ids(events), ids(evAgain))
}

func TestDecodeMetaNeverFails(t *testing.T) {
	require.Nil(t, decodeMeta(nil))
	require.Equal(t, map[string]string{"a": "b"}, decodeMeta([]byte(`{"a":"b"}`)))
	require.Equal(t, map[string]string{"a": "b", "n": "1", "o": `{"x":true}`}, decodeMeta([]byte(`{"a":"b","n":1,"o":{"x":true}}`)))
	require.Equal(t, map[string]string{"_meta": `[1,2]`}, decodeMeta([]byte(`[1,2]`)))
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

func TestBackfillOutboxRunsOnce(t *testing.T) {
	_, repo := getTestPool(t)
	ctx := context.Background()
	fill(t, repo, 4) // recorded before ClickHouse was turned on: no outbox rows
	repo.WithOutbox()
	require.Zero(t, backlog(t, repo))

	empty := func(context.Context) (bool, error) { return true, nil }
	n, err := repo.BackfillOutbox(ctx, empty)
	require.NoError(t, err)
	require.EqualValues(t, 4, n)
	require.EqualValues(t, 4, backlog(t, repo))

	n, err = repo.BackfillOutbox(ctx, empty)
	require.NoError(t, err)
	require.Zero(t, n, "a second start must not queue the history again")
}

func TestBackfillOutboxSkipsAFedClickHouse(t *testing.T) {
	_, repo := getTestPool(t)
	ctx := context.Background()
	fill(t, repo, 3)
	n, err := repo.BackfillOutbox(ctx, func(context.Context) (bool, error) { return false, nil })
	require.NoError(t, err)
	require.Zero(t, n, "ClickHouse already holds events: copying them again would count them twice")
	n, err = repo.BackfillOutbox(ctx, func(context.Context) (bool, error) { return true, nil })
	require.NoError(t, err)
	require.Zero(t, n, "the decision is made once")
}

func TestBackfillPendingUntilCopied(t *testing.T) {
	_, repo := getTestPool(t)
	ctx := context.Background()
	fill(t, repo, 2)
	repo.WithOutbox()
	pending, err := repo.BackfillPending(ctx)
	require.NoError(t, err)
	require.False(t, pending, "no backfill yet")

	_, err = repo.BackfillOutbox(ctx, func(context.Context) (bool, error) { return true, nil })
	require.NoError(t, err)
	fill(t, repo, 1) // recorded after the backfill: not part of it
	pending, err = repo.BackfillPending(ctx)
	require.NoError(t, err)
	require.True(t, pending)

	b, _, err := repo.ClaimOutbox(ctx, owner, 10, time.Minute)
	require.NoError(t, err)
	require.NoError(t, repo.AckOutbox(ctx, b.ID))
	pending, err = repo.BackfillPending(ctx)
	require.NoError(t, err)
	require.False(t, pending)
}
