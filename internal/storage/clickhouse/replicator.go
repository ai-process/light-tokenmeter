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
	refusedMax = 10 * time.Minute
)

// Outbox is the Postgres side: events written there in the same statement as
// the event itself, leased out in batches until ClickHouse has them.
type Outbox interface {
	ClaimOutbox(ctx context.Context, owner uuid.UUID, limit int, lease time.Duration) (repository.OutboxBatch, []*domain.UsageEvent, error)
	RenewOutbox(ctx context.Context, owner, batch uuid.UUID, lease time.Duration) (bool, error)
	AckOutbox(ctx context.Context, batch uuid.UUID) error
	RetryOutboxLater(ctx context.Context, owner, batch uuid.UUID, delay time.Duration) (bool, error)
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

// copyBatch copies one batch and reports whether to go on at once. While
// ClickHouse fails, the batch is held and retried; when ClickHouse refuses
// its rows, it is postponed so the events behind it flow.
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
			r.postpone(log, batch, len(events), err)
			return false // pace refusals: a broken schema refuses every batch
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

// refusesData reports ClickHouse refusing the rows, on the client while the
// batch is built, or on the server.
func refusesData(err error) bool {
	var bad *appendError
	var ex *clickhouse.Exception
	return errors.As(err, &bad) || errors.As(err, &ex) && dataErrorCodes[ex.Code]
}

// appendError is ClickHouse's client refusing a value before anything is sent.
type appendError struct{ err error }

func (e *appendError) Error() string { return e.err.Error() }
func (e *appendError) Unwrap() error { return e.err }

// postpone keeps the batch, under the same token, for a later retry. It is
// never split or dropped: a token reused for other rows could count twice.
func (r *Replicator) postpone(log zerolog.Logger, batch repository.OutboxBatch, n int, cause error) {
	attempts := batch.Attempts + 1
	delay := min(r.retryMax<<min(attempts, 6), refusedMax)
	log.Error().Err(cause).Int("events", n).Int("attempt", attempts).Dur("retry_in", delay).
		Msg("clickhouse replicator: ClickHouse refused the batch; it waits in the outbox and is retried later")
	if _, err := r.outbox.RetryOutboxLater(r.ctx, r.id, batch.ID, delay); err != nil && r.ctx.Err() == nil {
		log.Error().Err(err).Msg("clickhouse replicator: cannot postpone the batch")
	}
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

	for _, ev := range events {
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
			return &appendError{err: fmt.Errorf("append event %s to batch: %w", ev.EventID, err)}
		}
	}

	if err := b.Send(); err != nil {
		return fmt.Errorf("send batch to clickhouse: %w", err)
	}

	log.Debug().Int("events", len(events)).Str("batch", batch.String()).Msg("copied batch to clickhouse")
	return nil
}
