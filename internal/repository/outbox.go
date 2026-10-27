package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ai-process/light-tokenmeter/internal/domain"
)

// OutboxBatch is a claimed batch: its ID is ClickHouse's deduplication token,
// Attempts counts ClickHouse's refusals of it so far.
type OutboxBatch struct {
	ID       uuid.UUID
	Attempts int
}

// ClaimOutbox leases the next batch for ClickHouse. A batch whose lease ended
// (its replica died, or a refusal postponed it) comes first and keeps its ID
// for good, so ClickHouse drops a repeat of an insert it already stored.
func (r *Repository) ClaimOutbox(ctx context.Context, owner uuid.UUID, limit int, lease time.Duration) (OutboxBatch, []*domain.UsageEvent, error) {
	batch, ids, err := r.claimExpired(ctx, owner, lease)
	if err != nil {
		return OutboxBatch{}, nil, err
	}
	if batch.ID != uuid.Nil {
		events, err := r.eventsByID(ctx, ids)
		return batch, events, err
	}
	batch = OutboxBatch{ID: uuid.New()}
	// SKIP LOCKED lets replicas claim side by side; a row another claim just
	// took fails the batch_id IS NULL recheck and is skipped.
	rows, err := r.pool.Query(ctx, `
		WITH claimed AS (
			UPDATE clickhouse_outbox SET batch_id = $1, lease_until = now() + make_interval(secs => $3), lease_owner = $4
			WHERE event_id IN (
				SELECT event_id FROM clickhouse_outbox WHERE batch_id IS NULL
				ORDER BY enqueued_at LIMIT $2 FOR UPDATE SKIP LOCKED)
			RETURNING event_id)
		SELECT `+eventColumns+` FROM usage_events JOIN claimed USING (event_id)`, batch.ID, limit, lease.Seconds(), owner)
	if err != nil {
		return OutboxBatch{}, nil, fmt.Errorf("claim outbox batch: %w", err)
	}
	events, err := scanEvents(rows)
	if err != nil || len(events) == 0 {
		return OutboxBatch{}, nil, err
	}
	return batch, events, nil
}

// claimExpired takes over the batch whose lease ended longest ago. Replicas
// contending for one batch skip it via the advisory lock, and the lease
// recheck stops a replica that waited from taking a batch just renewed.
func (r *Repository) claimExpired(ctx context.Context, owner uuid.UUID, lease time.Duration) (OutboxBatch, []uuid.UUID, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT batch_id FROM clickhouse_outbox
		WHERE batch_id IS NOT NULL AND lease_until < now()
		GROUP BY batch_id ORDER BY min(lease_until) LIMIT 8`)
	if err != nil {
		return OutboxBatch{}, nil, fmt.Errorf("find expired outbox batches: %w", err)
	}
	candidates, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return OutboxBatch{}, nil, fmt.Errorf("find expired outbox batches: %w", err)
	}
	for _, id := range candidates {
		ids, attempts, err := r.renewIfExpired(ctx, owner, id, lease)
		if err != nil {
			return OutboxBatch{}, nil, err
		}
		if len(ids) > 0 {
			return OutboxBatch{ID: id, Attempts: attempts}, ids, nil
		}
	}
	return OutboxBatch{}, nil, nil
}

func (r *Repository) renewIfExpired(ctx context.Context, owner, batch uuid.UUID, lease time.Duration) ([]uuid.UUID, int, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("take over outbox batch: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var locked bool
	if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended($1::text, 0))`, batch).Scan(&locked); err != nil {
		return nil, 0, fmt.Errorf("take over outbox batch: %w", err)
	}
	if !locked {
		return nil, 0, nil
	}
	rows, err := tx.Query(ctx, `
		UPDATE clickhouse_outbox SET lease_until = now() + make_interval(secs => $2), lease_owner = $3
		WHERE event_id IN (`+batchRows+` AND lease_until < now() ORDER BY event_id FOR UPDATE)
		RETURNING event_id, attempts`, batch, lease.Seconds(), owner)
	if err != nil {
		return nil, 0, fmt.Errorf("take over outbox batch: %w", err)
	}
	var (
		ids      []uuid.UUID
		attempts int
	)
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id, &attempts); err != nil {
			rows.Close()
			return nil, 0, fmt.Errorf("take over outbox batch: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("take over outbox batch: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, 0, fmt.Errorf("take over outbox batch: %w", err)
	}
	return ids, attempts, nil
}

// batchRows selects a batch's rows for a statement to lock in event_id order:
// every statement on a batch locks in that order, so two never deadlock.
const batchRows = `SELECT event_id FROM clickhouse_outbox WHERE batch_id = $1`

// RenewOutbox extends the lease while ClickHouse is retried. False: the batch
// is gone or was taken over, and the caller must let it go.
func (r *Repository) RenewOutbox(ctx context.Context, owner, batch uuid.UUID, lease time.Duration) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		UPDATE clickhouse_outbox SET lease_until = now() + make_interval(secs => $2)
		WHERE event_id IN (`+batchRows+` AND lease_owner = $3 ORDER BY event_id FOR UPDATE)`,
		batch, lease.Seconds(), owner)
	if err != nil {
		return false, fmt.Errorf("renew outbox batch %s: %w", batch, err)
	}
	return tag.RowsAffected() > 0, nil
}

func (r *Repository) AckOutbox(ctx context.Context, batch uuid.UUID) error {
	if _, err := r.pool.Exec(ctx, `DELETE FROM clickhouse_outbox WHERE event_id IN (`+batchRows+` ORDER BY event_id FOR UPDATE)`, batch); err != nil {
		return fmt.Errorf("ack outbox batch %s: %w", batch, err)
	}
	return nil
}

// RetryOutboxLater counts a refusal and makes the batch claimable again after
// delay, so a refused batch waits while the events behind it flow. False: the
// batch was taken over meanwhile and is not the caller's to postpone.
func (r *Repository) RetryOutboxLater(ctx context.Context, owner, batch uuid.UUID, delay time.Duration) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		UPDATE clickhouse_outbox SET attempts = attempts + 1, lease_until = now() + make_interval(secs => $2), lease_owner = NULL
		WHERE event_id IN (`+batchRows+` AND lease_owner = $3 ORDER BY event_id FOR UPDATE)`, batch, delay.Seconds(), owner)
	if err != nil {
		return false, fmt.Errorf("postpone outbox batch %s: %w", batch, err)
	}
	return tag.RowsAffected() > 0, nil
}

// OutboxBacklog reports how many events wait for ClickHouse, counted up to
// a million so a large backlog stays cheap to sample, and the age of the oldest.
func (r *Repository) OutboxBacklog(ctx context.Context) (int64, time.Duration, error) {
	var (
		n     int64
		since *time.Time
		now   time.Time
	)
	err := r.pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM (SELECT 1 FROM clickhouse_outbox LIMIT 1000000) c),
			(SELECT enqueued_at FROM clickhouse_outbox ORDER BY enqueued_at LIMIT 1), now()`).Scan(&n, &since, &now)
	if err != nil {
		return 0, 0, fmt.Errorf("outbox backlog: %w", err)
	}
	if since == nil {
		return 0, 0, nil
	}
	return n, now.Sub(*since), nil
}

const eventColumns = `event_id, created_at, occurred_at, user_id, project_id, action_id,
	vendor, model, status, duration_ms, tokens_input, tokens_output, tokens_total,
	COALESCE(error_class, ''), COALESCE(vendor_http_status, 0), meta, cost_nanousd,
	cache_read_tokens, cache_write_tokens, tool_server, tool_name, response_bytes`

func (r *Repository) eventsByID(ctx context.Context, ids []uuid.UUID) ([]*domain.UsageEvent, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+eventColumns+` FROM usage_events WHERE event_id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("load outbox events: %w", err)
	}
	return scanEvents(rows)
}

func scanEvents(rows pgx.Rows) ([]*domain.UsageEvent, error) {
	defer rows.Close()
	var events []*domain.UsageEvent
	for rows.Next() {
		var (
			e      domain.UsageEvent
			status int16
			meta   []byte
		)
		if err := rows.Scan(&e.EventID, &e.CreatedAt, &e.OccurredAt, &e.UserID, &e.ProjectID, &e.ActionID,
			&e.Vendor, &e.Model, &status, &e.DurationMs, &e.TokensInput, &e.TokensOutput, &e.TokensTotal,
			&e.ErrorClass, &e.VendorHTTPStatus, &meta, &e.CostNanoUSD,
			&e.CacheReadTokens, &e.CacheWriteTokens, &e.ToolServer, &e.ToolName, &e.ResponseBytes); err != nil {
			return nil, fmt.Errorf("scan outbox event: %w", err)
		}
		e.Status = domain.Status(status)
		e.Meta = decodeMeta(meta)
		events = append(events, &e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load outbox events: %w", err)
	}
	return events, nil
}

// decodeMeta never fails a batch: a row written outside the API with
// non-string meta values gets them as JSON text rather than blocking the copy.
func decodeMeta(raw []byte) map[string]string {
	if len(raw) == 0 {
		return nil
	}
	var flat map[string]string
	if json.Unmarshal(raw, &flat) == nil {
		return flat
	}
	var loose map[string]json.RawMessage
	if json.Unmarshal(raw, &loose) != nil {
		return map[string]string{"_meta": string(raw)}
	}
	out := make(map[string]string, len(loose))
	for k, v := range loose {
		var str string
		if json.Unmarshal(v, &str) == nil {
			out[k] = str
		} else {
			out[k] = string(v)
		}
	}
	return out
}

// BackfillOutbox queues the stored events ClickHouse keeps (its 90-day TTL),
// once per database, when chEmpty says ClickHouse has none: history recorded
// before ClickHouse was turned on would otherwise be missing from its reports.
func (r *Repository) BackfillOutbox(ctx context.Context, chEmpty func(context.Context) (bool, error)) (int64, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("backfill outbox: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Replicas starting together take turns; the later ones find the marker.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('clickhouse_backfill', 0))`); err != nil {
		return 0, fmt.Errorf("backfill outbox: %w", err)
	}
	var done bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM clickhouse_backfill)`).Scan(&done); err != nil {
		return 0, fmt.Errorf("backfill outbox: %w", err)
	}
	if done {
		return 0, nil
	}
	empty, err := chEmpty(ctx)
	if err != nil {
		return 0, fmt.Errorf("backfill outbox: %w", err)
	}
	var queued int64
	if empty {
		// One-time and possibly large: a statement_timeout in DATABASE_URL
		// would roll it back and repeat it at every retry.
		if _, err := tx.Exec(ctx, `SET LOCAL statement_timeout = 0`); err != nil {
			return 0, fmt.Errorf("backfill outbox: %w", err)
		}
		tag, err := tx.Exec(ctx, `
			INSERT INTO clickhouse_outbox (event_id)
			SELECT event_id FROM usage_events
			WHERE COALESCE(occurred_at, created_at) > now() - interval '90 days'
			ON CONFLICT (event_id) DO NOTHING`)
		if err != nil {
			return 0, fmt.Errorf("backfill outbox: %w", err)
		}
		queued = tag.RowsAffected()
	}
	if _, err := tx.Exec(ctx, `INSERT INTO clickhouse_backfill (queued) VALUES ($1)`, queued); err != nil {
		return 0, fmt.Errorf("backfill outbox: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("backfill outbox: %w", err)
	}
	return queued, nil
}

// BackfillPending reports whether history queued by BackfillOutbox still waits
// for ClickHouse; until it is copied, reports must come from Postgres.
func (r *Repository) BackfillPending(ctx context.Context) (bool, error) {
	var pending bool
	err := r.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM clickhouse_outbox o, clickhouse_backfill b
			WHERE b.queued > 0 AND o.enqueued_at <= b.done_at)`).Scan(&pending)
	if err != nil {
		return false, fmt.Errorf("backfill pending: %w", err)
	}
	return pending, nil
}
