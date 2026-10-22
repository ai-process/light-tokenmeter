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
	"github.com/stretchr/testify/require"

	"github.com/ai-process/light-tokenmeter/internal/domain"
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
		waiting, _, _, err := s.repo.OutboxBacklog(context.Background())
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

	s.record(t, 500, "m")
	down := NewReplicator(s.repo, &flakyConn{Conn: s.ch.Conn(), fails: 3}, 200, 5*time.Millisecond)
	require.True(t, down.copyBatch(), "a full batch, once ClickHouse is back")

	batch, events, err := s.repo.ClaimOutbox(ctx, uuid.New(), 200, time.Hour)
	require.NoError(t, err)
	require.NoError(t, down.insert(ctx, batch.ID, events))
	// The process dies here, before AckOutbox; its lease runs out.
	expireLease(t, s, batch.ID)

	s.record(t, 700, "m")
	s.drain(t, NewReplicator(s.repo, s.ch.Conn(), 200, 5*time.Millisecond))
	require.EqualValues(t, 1200, pgTotals(t, s.repo, s.from, time.Now().Add(time.Hour)).requests)
	s.requireInStep(t, totals{})
}

func expireLease(t *testing.T, s *stack, batch uuid.UUID) {
	waiting, _, _, err := s.repo.OutboxBacklog(context.Background())
	require.NoError(t, err)
	require.NotZero(t, waiting)
	require.NoError(t, s.repo.RetryOutboxLater(context.Background(), batch, -time.Second))
}

// One event ClickHouse cannot take, refused by the client or by the server,
// is set aside; every other event, in its batch or behind it, gets through.
func TestPoisonEventIsSetAsideAndTheRestFlows(t *testing.T) {
	for _, server := range []bool{false, true} {
		name := "refused on append"
		if server {
			name = "refused by the server"
		}
		t.Run(name, func(t *testing.T) {
			s := newStack(t)
			s.record(t, 40, "m")
			s.record(t, 1, "poison")
			s.record(t, 40, "m")

			r := NewReplicator(s.repo, &flakyConn{Conn: s.ch.Conn(), poisonServer: server}, 32, time.Millisecond)
			r.retryMax = time.Millisecond // refusal delays become a few milliseconds
			s.drain(t, r)

			_, _, setAside, err := s.repo.OutboxBacklog(context.Background())
			require.NoError(t, err)
			require.EqualValues(t, 1, setAside)
			to := time.Now().Add(time.Hour)
			pg := pgTotals(t, s.repo, s.from, to)
			require.EqualValues(t, 81, pg.requests)
			// The poison event is the first of its record call: 0 input + 3 output tokens, cost 0.
			s.requireInStep(t, totals{requests: 1, tokens: 3})
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
	waiting, _, setAside, err := s.repo.OutboxBacklog(context.Background())
	require.NoError(t, err)
	require.EqualValues(t, 1, waiting, "the event stays in the outbox")
	require.Zero(t, setAside, "an outage never sets events aside")
}

// An outage is not the events' fault: however long it lasts, nothing is set aside.
func TestOutageNeverSetsEventsAside(t *testing.T) {
	s := newStack(t)
	s.record(t, 5, "m")
	conn := &flakyConn{Conn: s.ch.Conn(), fails: 20}
	r := NewReplicator(s.repo, conn, 5, time.Millisecond)
	r.retryMax = time.Millisecond
	s.drain(t, r)
	_, _, setAside, err := s.repo.OutboxBacklog(context.Background())
	require.NoError(t, err)
	require.Zero(t, setAside)
	s.requireInStep(t, totals{})
}

// The schema insert expects must be what EnsureSchema builds, or every
// refusal would be blamed on the schema and nothing ever set aside.
func TestInsertedColumnsMatchSchema(t *testing.T) {
	s := newStack(t)
	r := NewReplicator(s.repo, s.ch.Conn(), 10, time.Millisecond)
	mismatch, err := r.schemaMismatch()
	require.NoError(t, err)
	require.Empty(t, mismatch)
}

// With the schema changed under it, a ClickHouse refusing every row is not
// the rows' fault: they wait, none is set aside, and all flow once it is fixed.
func TestChangedSchemaNeverSetsEventsAside(t *testing.T) {
	s := newStack(t)
	ctx := context.Background()
	require.NoError(t, s.ch.Conn().Exec(ctx, "ALTER TABLE usage_events MODIFY COLUMN error_class String"))
	s.record(t, 6, "poison") // refused by the server, as a broken schema would

	conn := &flakyConn{Conn: s.ch.Conn(), poisonServer: true}
	r := NewReplicator(s.repo, conn, 2, time.Millisecond)
	r.retryMax = time.Millisecond
	for i := 0; i < 50; i++ {
		for r.copyBatch() {
		}
		time.Sleep(2 * time.Millisecond)
	}
	waiting, _, setAside, err := s.repo.OutboxBacklog(ctx)
	require.NoError(t, err)
	require.Zero(t, setAside, "a schema problem must not set events aside")
	require.EqualValues(t, 6, waiting)

	require.NoError(t, s.ch.Conn().Exec(ctx, "ALTER TABLE usage_events MODIFY COLUMN error_class LowCardinality(String)"))
	conn.heal()
	s.drain(t, r)
	s.requireInStep(t, totals{})
}
