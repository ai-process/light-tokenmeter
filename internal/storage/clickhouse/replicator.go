package clickhouse

import (
	"context"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"

	"github.com/ai-process/light-tokenmeter/internal/domain"
)

const (
	insertTimeout = 30 * time.Second
	// staleAfter must outlast an insert, or a live replica's batch is taken over.
	staleAfter = 2 * insertTimeout
	retryMax   = 30 * time.Second
)

// Outbox is the Postgres side: events written there in the same statement as
// the event itself, handed out in batches until ClickHouse has them.
type Outbox interface {
	ClaimOutbox(ctx context.Context, limit int, staleAfter time.Duration) (uuid.UUID, []*domain.UsageEvent, error)
	AckOutbox(ctx context.Context, batch uuid.UUID) error
}

// Replicator copies events from the Postgres outbox to ClickHouse. Nothing is
// held only in memory: while ClickHouse is down the outbox grows, and a
// restart resumes where it stopped.
type Replicator struct {
	outbox    Outbox
	conn      driver.Conn
	batchSize int
	interval  time.Duration
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
		ctx: ctx, cancel: cancel, done: make(chan struct{})}
}

// Start launches the copy loop.
func (r *Replicator) Start() { go r.run() }

// Close stops the loop. A batch it was copying stays claimed in the outbox and
// is copied, without duplicates, by the next replicator to find it stale.
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

// copyBatch copies one batch, retrying ClickHouse until it takes it, and
// reports whether a full batch suggests more are waiting.
func (r *Replicator) copyBatch() bool {
	ctx := r.ctx
	batch, events, err := r.outbox.ClaimOutbox(ctx, r.batchSize, staleAfter)
	if err != nil {
		if ctx.Err() == nil {
			log.Error().Err(err).Msg("clickhouse replicator: cannot read the outbox")
		}
		return false
	}
	if batch == uuid.Nil {
		return false
	}
	for backoff := r.interval; ; backoff = min(2*backoff, retryMax) {
		err := r.insert(ctx, batch, events)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return false
		}
		log.Warn().Err(err).Int("events", len(events)).Msg("clickhouse replicator: insert failed, events stay in the outbox and are retried")
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return false
		}
	}
	if err := r.outbox.AckOutbox(ctx, batch); err != nil {
		// The batch is retried once stale; ClickHouse skips it by its token.
		log.Error().Err(err).Msg("clickhouse replicator: cannot acknowledge a copied batch")
		return false
	}
	return len(events) >= r.batchSize
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
			return fmt.Errorf("append event to batch: %w", err)
		}
	}

	if err := b.Send(); err != nil {
		return fmt.Errorf("send batch to clickhouse: %w", err)
	}

	log.Debug().Int("events", len(events)).Str("batch", batch.String()).Msg("copied batch to clickhouse")
	return nil
}
