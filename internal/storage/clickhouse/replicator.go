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
	// lease outlasts an insert plus a renewal, so a live replica keeps its batch.
	lease      = 2 * time.Minute
	retryMax   = 15 * time.Second
	ackTimeout = 10 * time.Second
	// After this many refusals a batch is halved, and a single event set aside.
	maxAttempts = 3
	refusedMax  = 10 * time.Minute
)

// Outbox is the Postgres side: events written there in the same statement as
// the event itself, leased out in batches until ClickHouse has them.
type Outbox interface {
	ClaimOutbox(ctx context.Context, limit int, lease time.Duration) (repository.OutboxBatch, []*domain.UsageEvent, error)
	RenewOutbox(ctx context.Context, batch uuid.UUID, lease time.Duration) (bool, error)
	AckOutbox(ctx context.Context, batch uuid.UUID) error
	RetryOutboxLater(ctx context.Context, batch uuid.UUID, delay time.Duration) error
	SplitOutbox(ctx context.Context, batch uuid.UUID) error
	SetAsideOutbox(ctx context.Context, event uuid.UUID, reason string) error
}

// Replicator copies events from the Postgres outbox to ClickHouse. Nothing is
// held only in memory: while ClickHouse is down the outbox grows, and a
// restart resumes where it stopped.
type Replicator struct {
	outbox    Outbox
	conn      driver.Conn
	batchSize int
	interval  time.Duration
	retryMax  time.Duration   // overridden only by tests
	ctx       context.Context // cancelled by Close, cutting a claim or insert short
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
	return &Replicator{outbox: outbox, conn: conn, batchSize: batchSize, interval: interval,
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

// copyBatch copies one batch and reports whether to go on at once.
// ClickHouse unreachable: hold the batch, renewing its lease, and retry.
// ClickHouse refusing it: postpone it, halve it, and in the end set the event
// at fault aside, so one bad event never holds up the rest.
func (r *Replicator) copyBatch() bool {
	batch, events, err := r.outbox.ClaimOutbox(r.ctx, r.batchSize, lease)
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
	for backoff := r.interval; ; {
		err := r.insert(r.ctx, batch.ID, events)
		if err == nil {
			break
		}
		if r.ctx.Err() != nil {
			return false // closing: the batch is copied once its lease ends
		}

		var bad *appendError
		if errors.As(err, &bad) {
			ev := events[bad.index]
			log.Error().Err(bad.err).Str("event_id", ev.EventID.String()).Msg("clickhouse replicator: ClickHouse cannot take this event; set aside")
			if err := r.outbox.SetAsideOutbox(r.ctx, ev.EventID, bad.err.Error()); err != nil {
				log.Error().Err(err).Msg("clickhouse replicator: cannot set an event aside")
				return false
			}
			events = append(events[:bad.index], events[bad.index+1:]...)
			continue // nothing was sent, so the token is still unused
		}

		if refusesData(err) {
			return r.refused(log, batch, events, err)
		}

		if backoff == r.interval {
			log.Warn().Err(err).Int("events", len(events)).Msg("clickhouse replicator: insert failed, holding the batch and retrying")
		}
		select {
		case <-time.After(backoff):
		case <-r.ctx.Done():
			return false
		}
		backoff = min(2*backoff, r.retryMax)
		if ok, err := r.outbox.RenewOutbox(r.ctx, batch.ID, lease); err != nil || !ok {
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
	return len(events) >= r.batchSize
}

// dataErrorCodes are ClickHouse errors about the rows themselves. Any other
// error (schema, permissions, limits) is treated as an outage and retried, so
// a broken ClickHouse never gets events set aside.
var dataErrorCodes = map[int32]bool{
	6: true, 27: true, 38: true, 41: true, 53: true, 69: true, 70: true,
	72: true, 117: true, 131: true, 321: true, 349: true,
}

func refusesData(err error) bool {
	var ex *clickhouse.Exception
	return errors.As(err, &ex) && dataErrorCodes[ex.Code]
}

// refused handles ClickHouse answering with an error: nothing was stored under
// the token, so the batch may be postponed, split or narrowed freely.
func (r *Replicator) refused(log zerolog.Logger, batch repository.OutboxBatch, events []*domain.UsageEvent, cause error) bool {
	attempts := batch.Attempts + 1
	var err error
	switch {
	case attempts < maxAttempts:
		delay := min(r.retryMax<<attempts, refusedMax)
		log.Warn().Err(cause).Int("attempt", attempts).Dur("retry_in", delay).Msg("clickhouse replicator: ClickHouse refused the batch; retrying later")
		err = r.outbox.RetryOutboxLater(r.ctx, batch.ID, delay)
	case len(events) > 1:
		log.Warn().Err(cause).Int("events", len(events)).Msg("clickhouse replicator: ClickHouse keeps refusing the batch; splitting it")
		err = r.outbox.SplitOutbox(r.ctx, batch.ID)
	case len(events) == 1:
		log.Error().Err(cause).Str("event_id", events[0].EventID.String()).Msg("clickhouse replicator: ClickHouse refuses this event; set aside")
		err = r.outbox.SetAsideOutbox(r.ctx, events[0].EventID, cause.Error())
	default:
		err = r.outbox.AckOutbox(r.ctx, batch.ID)
	}
	if err != nil {
		if r.ctx.Err() == nil {
			log.Error().Err(err).Msg("clickhouse replicator: cannot record the refusal")
		}
		return false
	}
	return true
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
