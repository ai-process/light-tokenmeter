package clickhouse

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/ai-process/light-tokenmeter/internal/domain"
	"github.com/ai-process/light-tokenmeter/internal/repository"
	"github.com/ai-process/light-tokenmeter/internal/testdb"
)

// flakyConn fails the first fails inserts, like ClickHouse during an outage.
type flakyConn struct {
	driver.Conn
	mu    sync.Mutex
	fails int
}

func (c *flakyConn) setFails(n int) {
	c.mu.Lock()
	c.fails = n
	c.mu.Unlock()
}

func (c *flakyConn) PrepareBatch(ctx context.Context, query string, opts ...driver.PrepareBatchOption) (driver.Batch, error) {
	c.mu.Lock()
	down := c.fails > 0
	if down {
		c.fails--
	}
	c.mu.Unlock()
	if down {
		return nil, errors.New("connection refused")
	}
	return c.Conn.PrepareBatch(ctx, query, opts...)
}

type totals struct{ requests, tokens, cost int64 }

func pgTotals(t *testing.T, repo *repository.Repository, from, to time.Time) totals {
	rows, _, err := repo.QuerySummary(context.Background(), domain.SummaryParams{From: from, To: to})
	require.NoError(t, err)
	var out totals
	for _, r := range rows {
		out.requests += r.RequestCount
		out.tokens += r.TokensTotal
		out.cost += r.CostNanoUSD
	}
	return out
}

func chTotals(t *testing.T, c *Client, from, to time.Time) totals {
	rows, _, err := c.QuerySummary(context.Background(), domain.SummaryParams{From: from, To: to})
	require.NoError(t, err)
	var out totals
	for _, r := range rows {
		out.requests += r.RequestCount
		out.tokens += r.TokensTotal
		out.cost += r.CostNanoUSD
	}
	return out
}

// Events recorded during a ClickHouse outage, a batch copied but never
// acknowledged (the process died), and a restart: Postgres and ClickHouse
// totals still agree, with nothing lost and nothing counted twice.
func TestReplicatorKeepsClickHouseInStepWithPostgres(t *testing.T) {
	ch := getTestClient(t)
	pool := testdb.Postgres(t)
	if ch == nil || pool == nil {
		return
	}
	repo := repository.New(pool).WithOutbox()
	ctx := context.Background()
	from := time.Now().Add(-time.Hour)

	record := func(n int) {
		batch := make([]*domain.UsageEvent, 0, n)
		for i := 0; i < n; i++ {
			at := time.Now().UTC()
			batch = append(batch, &domain.UsageEvent{
				EventID: uuid.New(), OccurredAt: &at, UserID: "u", ProjectID: "p", ActionID: "a",
				Vendor: "openai", Model: "m", Status: domain.StatusSuccess,
				TokensInput: int64(i % 7), TokensOutput: 3, TokensTotal: int64(i%7) + 3, CostNanoUSD: int64(i),
			})
		}
		_, err := repo.InsertBatch(ctx, batch)
		require.NoError(t, err)
	}

	// ClickHouse is down for the first three inserts; the replicator retries
	// the same batch and the outbox keeps everything meanwhile.
	record(500)
	down := NewReplicator(repo, &flakyConn{Conn: ch.Conn(), fails: 3}, 200, 5*time.Millisecond)
	require.True(t, down.copyBatch(), "a full batch, once ClickHouse is back")

	// A batch reaches ClickHouse but the process dies before acknowledging it.
	batch, events, err := repo.ClaimOutbox(ctx, 200, time.Hour)
	require.NoError(t, err)
	require.NoError(t, down.insert(ctx, batch, events))
	_, err = pool.Exec(ctx, "UPDATE clickhouse_outbox SET claimed_at = now() - interval '1 hour' WHERE batch_id = $1", batch)
	require.NoError(t, err)

	// After the restart more events arrive while the replicator catches up.
	record(700)
	restarted := NewReplicator(repo, ch.Conn(), 200, 5*time.Millisecond)
	for restarted.copyBatch() {
	}
	for {
		n, _, err := repo.OutboxBacklog(ctx)
		require.NoError(t, err)
		if n == 0 {
			break
		}
		restarted.copyBatch()
	}

	to := time.Now().Add(time.Hour)
	want := pgTotals(t, repo, from, to)
	require.EqualValues(t, 1200, want.requests)
	require.Equal(t, want, chTotals(t, ch, from, to), "ClickHouse totals must match Postgres")
}

// A batch ClickHouse keeps refusing is given up after holdFor: the events
// behind it still flow, and it is copied once stale and accepted.
func TestRefusedBatchDoesNotStallReplication(t *testing.T) {
	ch := getTestClient(t)
	pool := testdb.Postgres(t)
	if ch == nil || pool == nil {
		return
	}
	repo := repository.New(pool).WithOutbox()
	ctx := context.Background()
	from := time.Now().Add(-time.Hour)
	add := func() {
		at := time.Now().UTC()
		_, err := repo.InsertOne(ctx, &domain.UsageEvent{
			EventID: uuid.New(), OccurredAt: &at, UserID: "u", ProjectID: "p", ActionID: "a",
			Vendor: "openai", Model: "m", Status: domain.StatusSuccess, TokensTotal: 5, CostNanoUSD: 7,
		})
		require.NoError(t, err)
	}
	for i := 0; i < 3; i++ {
		add()
	}

	conn := &flakyConn{Conn: ch.Conn(), fails: 1 << 30}
	r := NewReplicator(repo, conn, 3, 5*time.Millisecond)
	r.holdFor, r.stale = 50*time.Millisecond, time.Hour
	start := time.Now()
	require.False(t, r.copyBatch(), "the refused batch is given up")
	require.Less(t, time.Since(start), 5*time.Second)

	conn.setFails(0)
	for i := 0; i < 2; i++ {
		add()
	}
	require.False(t, r.copyBatch(), "newer events are copied past the refused batch")
	n, _, err := repo.OutboxBacklog(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 3, n, "only the refused batch waits")

	r.stale = 0
	r.copyBatch()
	n, _, err = repo.OutboxBacklog(ctx)
	require.NoError(t, err)
	require.Zero(t, n)
	to := time.Now().Add(time.Hour)
	require.Equal(t, pgTotals(t, repo, from, to), chTotals(t, ch, from, to))
}

// Close cuts a retry loop short and leaves the batch claimed for the next start.
func TestReplicatorCloseDuringOutage(t *testing.T) {
	ch := getTestClient(t)
	pool := testdb.Postgres(t)
	if ch == nil || pool == nil {
		return
	}
	repo := repository.New(pool).WithOutbox()
	at := time.Now().UTC()
	_, err := repo.InsertOne(context.Background(), &domain.UsageEvent{
		EventID: uuid.New(), OccurredAt: &at, UserID: "u", ProjectID: "p", ActionID: "a",
		Vendor: "openai", Model: "m", Status: domain.StatusSuccess, TokensTotal: 0,
	})
	require.NoError(t, err)

	r := NewReplicator(repo, &flakyConn{Conn: ch.Conn(), fails: 1 << 30}, 10, 5*time.Millisecond)
	r.Start()
	time.Sleep(50 * time.Millisecond)
	done := make(chan struct{})
	go func() { r.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not stop a replicator stuck on a down ClickHouse")
	}
	n, _, err := repo.OutboxBacklog(context.Background())
	require.NoError(t, err)
	require.EqualValues(t, 1, n, "the event stays in the outbox")
}
