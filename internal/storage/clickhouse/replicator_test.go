package clickhouse

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/ai-process/light-tokenmeter/internal/domain"
	"github.com/ai-process/light-tokenmeter/internal/metrics"
	"github.com/ai-process/light-tokenmeter/internal/repository"
	"github.com/ai-process/light-tokenmeter/internal/testdb"
)

// flakyConn stands in for a troubled ClickHouse: it can be unreachable for
// the next fails inserts, and refuse rows of model "poison" the way the
// client (on Append) or the server (on Send) would.
type flakyConn struct {
	driver.Conn
	mu           sync.Mutex
	fails        int
	poisonServer bool // refuse on Send with a data error, not on Append
	healed       bool // poison is accepted like any row
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
	b, err := c.Conn.PrepareBatch(ctx, query, opts...)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.healed {
		return b, nil
	}
	return &poisonBatch{Batch: b, server: c.poisonServer}, nil
}

func (c *flakyConn) heal() {
	c.mu.Lock()
	c.healed = true
	c.mu.Unlock()
}

type poisonBatch struct {
	driver.Batch
	server   bool
	poisoned bool
}

func (b *poisonBatch) Append(v ...any) error {
	if v[8] == "poison" { // model
		if !b.server {
			return errors.New("cannot convert value")
		}
		b.poisoned = true
	}
	return b.Batch.Append(v...)
}

func (b *poisonBatch) Send() error {
	if b.poisoned {
		_ = b.Batch.Abort()
		return &clickhouse.Exception{Code: 53, Message: "Type mismatch"}
	}
	return b.Batch.Send()
}

type totals struct{ requests, tokens, cost, cacheRead int64 }

func pgTotals(t *testing.T, repo *repository.Repository, from, to time.Time) totals {
	rows, _, err := repo.QuerySummary(context.Background(), domain.SummaryParams{From: from, To: to})
	require.NoError(t, err)
	var out totals
	for _, r := range rows {
		out.requests += r.RequestCount
		out.tokens += r.TokensTotal
		out.cost += r.CostNanoUSD
		out.cacheRead += r.CacheReadTokens
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
		out.cacheRead += r.CacheReadTokens
	}
	return out
}

type stack struct {
	ch   *Client
	repo *repository.Repository
	from time.Time
}

func newStack(t *testing.T) *stack {
	ch := getTestClient(t)
	pool := testdb.Postgres(t)
	if ch == nil || pool == nil {
		t.SkipNow()
	}
	return &stack{ch: ch, repo: repository.New(pool).WithOutbox(), from: time.Now().Add(-time.Hour)}
}

func (s *stack) record(t *testing.T, n int, model string) {
	batch := make([]*domain.UsageEvent, 0, n)
	for i := 0; i < n; i++ {
		at := time.Now().UTC()
		batch = append(batch, &domain.UsageEvent{
			EventID: uuid.New(), OccurredAt: &at, UserID: "u", ProjectID: "p", ActionID: "a",
			Vendor: "openai", Model: model, Status: domain.StatusSuccess,
			TokensInput: int64(i % 7), TokensOutput: 3, TokensTotal: int64(i%7) + 3, CostNanoUSD: int64(i),
			CacheReadTokens: int64(i % 7 / 2),
		})
	}
	_, err := s.repo.InsertBatch(context.Background(), batch)
	require.NoError(t, err)
}

// drain copies until nothing waits, as the loop would over several ticks.
func (s *stack) drain(t *testing.T, r *Replicator) {
	for i := 0; i < 1000; i++ {
		for r.copyBatch() {
		}
		waiting, _, err := s.repo.OutboxBacklog(context.Background())
		require.NoError(t, err)
		if waiting == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the outbox never drained")
}

func (s *stack) requireInStep(t *testing.T, without totals) {
	to := time.Now().Add(time.Hour)
	pg := pgTotals(t, s.repo, s.from, to)
	pg.requests, pg.tokens, pg.cost = pg.requests-without.requests, pg.tokens-without.tokens, pg.cost-without.cost
	require.Equal(t, pg, chTotals(t, s.ch, s.from, to), "ClickHouse must hold exactly what Postgres holds")
}

// Events recorded during a ClickHouse outage, a batch copied but never
// acknowledged (the process died), and a restart: Postgres and ClickHouse
// totals still agree, with nothing lost and nothing counted twice.
func TestReplicatorKeepsClickHouseInStepWithPostgres(t *testing.T) {
	s := newStack(t)
	ctx := context.Background()

	copied0 := testutil.ToFloat64(metrics.Copied)
	unreachable0 := testutil.ToFloat64(metrics.InsertFailures.WithLabelValues("unreachable"))
	s.record(t, 500, "m")
	down := NewReplicator(s.repo, &flakyConn{Conn: s.ch.Conn(), fails: 3}, 200, 5*time.Millisecond)
	require.True(t, down.copyBatch(), "a full batch, once ClickHouse is back")

	crashed := uuid.New()
	batch, events, err := s.repo.ClaimOutbox(ctx, crashed, 200, time.Hour)
	require.NoError(t, err)
	require.NoError(t, down.insert(ctx, batch.ID, events))
	// The process dies here, before AckOutbox; its lease runs out.
	expireLease(t, s, crashed, batch.ID)

	s.record(t, 700, "m")
	s.drain(t, NewReplicator(s.repo, s.ch.Conn(), 200, 5*time.Millisecond))
	require.EqualValues(t, 1200, pgTotals(t, s.repo, s.from, time.Now().Add(time.Hour)).requests)
	s.requireInStep(t, totals{})
	// The crashed insert bypassed copyBatch, so each event is counted once.
	require.Equal(t, copied0+1200, testutil.ToFloat64(metrics.Copied))
	require.Equal(t, unreachable0+3, testutil.ToFloat64(metrics.InsertFailures.WithLabelValues("unreachable")))
}

func expireLease(t *testing.T, s *stack, owner, batch uuid.UUID) {
	ok, err := s.repo.RetryOutboxLater(context.Background(), owner, batch, -time.Second)
	require.NoError(t, err)
	require.True(t, ok)
}

// A batch ClickHouse refuses, on the client or on the server, waits under
// its token while the events behind it flow; once ClickHouse takes it, the
// totals agree with nothing counted twice.
func TestRefusedBatchWaitsAndTheRestFlows(t *testing.T) {
	for _, server := range []bool{false, true} {
		name := "refused on append"
		if server {
			name = "refused by the server"
		}
		t.Run(name, func(t *testing.T) {
			s := newStack(t)
			s.record(t, 1, "poison")
			s.record(t, 40, "m")
			conn := &flakyConn{Conn: s.ch.Conn(), poisonServer: server}
			r := NewReplicator(s.repo, conn, 32, time.Millisecond)
			r.retryMax = time.Millisecond // postponements become a few milliseconds
			for i := 0; i < 30; i++ {
				for r.copyBatch() {
				}
				time.Sleep(2 * time.Millisecond)
			}
			waiting, _, err := s.repo.OutboxBacklog(context.Background())
			require.NoError(t, err)
			require.NotZero(t, waiting, "the refused batch waits")
			require.Less(t, waiting, int64(41), "events behind it got through")

			conn.heal()
			s.drain(t, r)
			s.requireInStep(t, totals{})
		})
	}
}

// Close cuts a retry loop short and leaves the batch leased for the next start.
func TestReplicatorCloseDuringOutage(t *testing.T) {
	s := newStack(t)
	s.record(t, 1, "m")

	r := NewReplicator(s.repo, &flakyConn{Conn: s.ch.Conn(), fails: 1 << 30}, 10, 5*time.Millisecond)
	r.Start()
	time.Sleep(50 * time.Millisecond)
	done := make(chan struct{})
	go func() { r.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not stop a replicator stuck on a down ClickHouse")
	}
	waiting, _, err := s.repo.OutboxBacklog(context.Background())
	require.NoError(t, err)
	require.EqualValues(t, 1, waiting, "the event stays in the outbox")
}

// A long outage only delays: the batch is held, then copied once.
func TestLongOutageOnlyDelays(t *testing.T) {
	s := newStack(t)
	s.record(t, 5, "m")
	r := NewReplicator(s.repo, &flakyConn{Conn: s.ch.Conn(), fails: 20}, 5, time.Millisecond)
	r.retryMax = time.Millisecond
	s.drain(t, r)
	s.requireInStep(t, totals{})
}

// A batch that keeps failing for reasons not about its rows is postponed
// after holdMax, so the events behind it still flow.
func TestStuckBatchIsPostponedAfterHoldMax(t *testing.T) {
	s := newStack(t)
	s.record(t, 3, "m")
	stuck := &flakyConn{Conn: s.ch.Conn(), fails: 1 << 30}
	r := NewReplicator(s.repo, stuck, 3, time.Millisecond)
	r.retryMax, r.holdMax = time.Millisecond, 20*time.Millisecond
	start := time.Now()
	require.False(t, r.copyBatch())
	require.Less(t, time.Since(start), 5*time.Second)

	s.record(t, 3, "m")
	ok := NewReplicator(s.repo, s.ch.Conn(), 3, time.Millisecond)
	ok.retryMax = time.Millisecond
	require.True(t, ok.copyBatch(), "the batch behind the postponed one is copied")
	s.drain(t, ok)
	s.requireInStep(t, totals{})
}
