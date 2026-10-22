package clickhouse

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/ai-process/light-tokenmeter/internal/domain"
	"github.com/ai-process/light-tokenmeter/internal/repository"
)

const (
	insertTimeout = 20 * time.Second
	// lease outlasts the longest wait between renewals (retryMax) many times.
	lease      = 2 * time.Minute
	retryMax   = 15 * time.Second
	ackTimeout = 10 * time.Second
	// After this many refusals a batch is halved, and a single event set aside.
	maxAttempts = 3
	refusedMax  = 10 * time.Minute
	// A refusal blames the rows only if the schema is as expected, or
	// ClickHouse took another insert lately.
	acceptedWithin = 5 * time.Minute
)

// insertedColumns are the types insert writes; the expected schema.
var insertedColumns = map[string]string{
	"event_id": "UUID", "created_at": "DateTime64(3)", "occurred_at": "Nullable(DateTime64(3))",
	"timestamp": "DateTime64(3)", "user_id": "String", "project_id": "String", "action_id": "String",
	"vendor": "LowCardinality(String)", "model": "LowCardinality(String)", "status": "Int16",
	"duration_ms": "Int64", "tokens_input": "Int64", "tokens_output": "Int64", "tokens_total": "Int64",
	"error_class": "LowCardinality(String)", "vendor_http_status": "Nullable(Int32)",
	"meta": "Map(String, String)", "cost_nanousd": "Int64",
}

// Outbox is the Postgres side: events written there in the same statement as
// the event itself, leased out in batches until ClickHouse has them.
type Outbox interface {
	ClaimOutbox(ctx context.Context, owner uuid.UUID, limit int, lease time.Duration) (repository.OutboxBatch, []*domain.UsageEvent, error)
	RenewOutbox(ctx context.Context, owner, batch uuid.UUID, lease time.Duration) (bool, error)
	AckOutbox(ctx context.Context, batch uuid.UUID) error
	RetryOutboxLater(ctx context.Context, batch uuid.UUID, delay time.Duration) error
	SplitOutbox(ctx context.Context, batch uuid.UUID, attempts int) error
	SetAsideOutbox(ctx context.Context, event uuid.UUID, reason string) error
}

// Replicator copies events from the Postgres outbox to ClickHouse. Nothing is
// held only in memory: while ClickHouse is down the outbox grows, and a
// restart resumes where it stopped.
type Replicator struct {
	outbox    Outbox
	conn      driver.Conn
	id        uuid.UUID // owns the leases this replicator takes
	batchSize int
	interval  time.Duration
	retryMax  time.Duration // overridden only by tests
	accepted  time.Time     // last insert ClickHouse took
	ctx       context.Context
	cancel    context.CancelFunc
	done      chan struct{}
}

// NewReplicator copies up to batchSize events per insert, checking the
// outbox every interval.
func NewReplicator(outbox Outbox, conn driver.Conn, batchSize int, interval time.Duration) *Replicator {
	if batchSize <= 0 {
		batchSize = 5000
	}
	if interval <= 0 {
		interval = time.Second
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Replicator{outbox: outbox, conn: conn, id: uuid.New(), batchSize: batchSize, interval: interval,
		retryMax: retryMax, ctx: ctx, cancel: cancel, done: make(chan struct{})}
}

func (r *Replicator) Start() { go r.run() }

// Close stops the loop. A batch it was copying stays leased in the outbox and
// is copied, without duplicates, by whoever claims it once the lease ends.
func (r *Replicator) Close() {
	r.cancel()
	<-r.done
}

func (r *Replicator) run() {
	defer close(r.done)
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		for r.copyBatch() {
		}
		select {
		case <-ticker.C:
		case <-r.ctx.Done():
			return
		}
	}
}

// appendError is ClickHouse's client refusing one event before anything is sent.
type appendError struct {
	index int
	err   error
}

func (e *appendError) Error() string { return e.err.Error() }
func (e *appendError) Unwrap() error { return e.err }

// copyBatch copies one batch and reports whether to go on at once. While
// ClickHouse fails, it holds the batch and retries; a refusal of the rows
// goes to refused, which postpones, halves or sets aside.
func (r *Replicator) copyBatch() bool {
	batch, events, err := r.outbox.ClaimOutbox(r.ctx, r.id, r.batchSize, lease)
	if err != nil {
		if r.ctx.Err() == nil {
			log.Error().Err(err).Msg("clickhouse replicator: cannot read the outbox")
		}
		return false
	}
	if batch.ID == uuid.Nil {
		return false
	}
	log := log.With().Str("batch", batch.ID.String()).Logger()
	backoff := min(r.interval, r.retryMax)
	for warned := false; ; {
		err := r.insert(r.ctx, batch.ID, events)
		if err == nil {
			break
		}
		if r.ctx.Err() != nil {
			return false // closing: the batch is copied once its lease ends
		}
		if refusesData(err) {
			return r.refused(log, batch, events, err)
		}
		if !warned {
			log.Warn().Err(err).Int("events", len(events)).Msg("clickhouse replicator: insert failed, holding the batch and retrying")
			warned = true
		}
		select {
		case <-time.After(backoff):
		case <-r.ctx.Done():
			return false
		}
		backoff = min(2*backoff, r.retryMax)
		if ok, err := r.outbox.RenewOutbox(r.ctx, r.id, batch.ID, lease); err != nil || !ok {
			if err != nil && r.ctx.Err() == nil {
				log.Error().Err(err).Msg("clickhouse replicator: cannot renew the lease")
			}
			return false
		}
	}
	r.accepted = time.Now()
	// Not r.ctx: a Close right after the insert must still record it.
	ackCtx, cancel := context.WithTimeout(context.Background(), ackTimeout)
	defer cancel()
	if err := r.outbox.AckOutbox(ackCtx, batch.ID); err != nil {
		log.Error().Err(err).Msg("clickhouse replicator: cannot acknowledge a copied batch; ClickHouse skips it when retried")
		return false
	}
	return true
}

// dataErrorCodes are ClickHouse errors about the values being inserted. Any
// other error (connection, permissions, limits) is retried like an outage.
var dataErrorCodes = map[int32]bool{
	6: true, 27: true, 38: true, 41: true, 53: true, 69: true, 70: true,
	72: true, 117: true, 131: true, 321: true, 349: true,
}

func refusesData(err error) bool {
	var bad *appendError
	var ex *clickhouse.Exception
	return errors.As(err, &bad) || errors.As(err, &ex) && dataErrorCodes[ex.Code]
}

// refused handles ClickHouse refusing the rows: nothing was stored under the
// token, so the batch may be postponed, split or narrowed. Unless ClickHouse
// takes other inserts, the schema is suspect, and the batch only waits.
func (r *Replicator) refused(log zerolog.Logger, batch repository.OutboxBatch, events []*domain.UsageEvent, cause error) bool {
	attempts := batch.Attempts + 1
	others := !r.accepted.IsZero() && time.Since(r.accepted) < acceptedWithin
	if !others {
		mismatch, err := r.schemaMismatch()
		if err != nil {
			log.Warn().Err(err).Msg("clickhouse replicator: cannot read the ClickHouse schema")
		}
		others = err == nil && mismatch == ""
		if mismatch != "" {
			cause = fmt.Errorf("%w (ClickHouse schema: %s)", cause, mismatch)
		}
	}
	var err error
	switch {
	case !others:
		delay := min(r.retryMax<<min(attempts, 6), refusedMax)
		log.Error().Err(cause).Dur("retry_in", delay).Msg("clickhouse replicator: ClickHouse refuses the batch and its schema is not as expected; waiting")
		err = r.outbox.RetryOutboxLater(r.ctx, batch.ID, delay)
	case attempts < maxAttempts:
		delay := min(r.retryMax<<attempts, refusedMax)
		log.Warn().Err(cause).Int("attempt", attempts).Dur("retry_in", delay).Msg("clickhouse replicator: ClickHouse refused the batch; retrying later")
		err = r.outbox.RetryOutboxLater(r.ctx, batch.ID, delay)
	case len(events) > 1:
		log.Warn().Err(cause).Int("events", len(events)).Msg("clickhouse replicator: ClickHouse keeps refusing the batch; splitting it")
		err = r.outbox.SplitOutbox(r.ctx, batch.ID, maxAttempts-1)
	case len(events) == 1:
		log.Error().Err(cause).Str("event_id", events[0].EventID.String()).Msg("clickhouse replicator: ClickHouse refuses this event; set aside")
		err = r.outbox.SetAsideOutbox(r.ctx, events[0].EventID, cause.Error())
	default:
		err = r.outbox.AckOutbox(r.ctx, batch.ID)
	}
	if err != nil && r.ctx.Err() == nil {
		log.Error().Err(err).Msg("clickhouse replicator: cannot record the refusal")
	}
	// Pause after a suspect schema instead of claiming the whole outbox in a burst.
	return err == nil && others
}

// schemaMismatch names the first usage_events column whose type differs from
// what insert writes, or "" when all match.
func (r *Replicator) schemaMismatch() (string, error) {
	ctx, cancel := context.WithTimeout(r.ctx, insertTimeout)
	defer cancel()
	rows, err := r.conn.Query(ctx, "SELECT name, type FROM system.columns WHERE database = currentDatabase() AND table = 'usage_events'")
	if err != nil {
		return "", err
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var name, typ string
		if err := rows.Scan(&name, &typ); err != nil {
			return "", err
		}
		got[name] = typ
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	for name, want := range insertedColumns {
		if got[name] != want {
			return fmt.Sprintf("column %s is %q, want %q", name, got[name], want), nil
		}
	}
	return "", nil
}

// insert writes one batch under its deduplication token: a batch ClickHouse
// already stored, retried after a timeout or a crash, is skipped.
func (r *Replicator) insert(ctx context.Context, batch uuid.UUID, events []*domain.UsageEvent) error {
	if len(events) == 0 {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, insertTimeout)
	defer cancel()
	ctx = clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{
		"insert_deduplicate":         1,
		"insert_deduplication_token": batch.String(),
	}))

	b, err := r.conn.PrepareBatch(ctx, `
		INSERT INTO usage_events (
			event_id, created_at, occurred_at, timestamp,
			user_id, project_id, action_id,
			vendor, model, status, duration_ms,
			tokens_input, tokens_output, tokens_total,
			error_class, vendor_http_status, meta, cost_nanousd
		)
	`)
	if err != nil {
		return fmt.Errorf("prepare clickhouse batch: %w", err)
	}

	for i, ev := range events {
		createdAt := ev.CreatedAt
		if createdAt.IsZero() {
			createdAt = time.Now()
		}

		ts := createdAt
		if ev.OccurredAt != nil {
			ts = *ev.OccurredAt
		}

		var vendorStatus *int32
		if ev.VendorHTTPStatus != 0 {
			v := ev.VendorHTTPStatus
			vendorStatus = &v
		}

		meta := ev.Meta
		if meta == nil {
			meta = map[string]string{}
		}

		err := b.Append(
			ev.EventID,
			createdAt,
			ev.OccurredAt,
			ts,
			ev.UserID,
			ev.ProjectID,
			ev.ActionID,
			ev.Vendor,
			ev.Model,
			int16(ev.Status),
			ev.DurationMs,
			ev.TokensInput,
			ev.TokensOutput,
			ev.TokensTotal,
			ev.ErrorClass,
			vendorStatus,
			meta,
			ev.CostNanoUSD,
		)
		if err != nil {
			_ = b.Abort()
			return &appendError{index: i, err: fmt.Errorf("append event to batch: %w", err)}
		}
	}

	if err := b.Send(); err != nil {
		return fmt.Errorf("send batch to clickhouse: %w", err)
	}

	log.Debug().Int("events", len(events)).Str("batch", batch.String()).Msg("copied batch to clickhouse")
	return nil
}
