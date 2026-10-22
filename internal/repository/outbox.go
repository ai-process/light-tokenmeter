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
// (its replica died, or a refusal set it aside for later) comes first and
// keeps its ID, so ClickHouse drops a repeat of an insert it already stored.
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
				SELECT event_id FROM clickhouse_outbox WHERE batch_id IS NULL AND failed_at IS NULL
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
// delay, so a refused batch waits while the events behind it flow.
func (r *Repository) RetryOutboxLater(ctx context.Context, batch uuid.UUID, delay time.Duration) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE clickhouse_outbox SET attempts = attempts + 1, lease_until = now() + make_interval(secs => $2), lease_owner = NULL
		WHERE event_id IN (`+batchRows+` ORDER BY event_id FOR UPDATE)`, batch, delay.Seconds())
	if err != nil {
		return fmt.Errorf("postpone outbox batch %s: %w", batch, err)
	}
	return nil
}

// SplitOutbox halves a batch ClickHouse keeps refusing, to corner the event at
// fault; the halves keep attempts. Fresh IDs are safe: ClickHouse stored
// nothing under a refused token.
func (r *Repository) SplitOutbox(ctx context.Context, batch uuid.UUID, attempts int) error {
	_, err := r.pool.Exec(ctx, `
		WITH locked AS (`+batchRows+` ORDER BY event_id FOR UPDATE),
		ranked AS (
			SELECT event_id, row_number() OVER (ORDER BY event_id) <= count(*) OVER () / 2 AS first_half
			FROM locked)
		UPDATE clickhouse_outbox o
		SET batch_id = CASE WHEN ranked.first_half THEN $2::uuid ELSE $3::uuid END,
			attempts = $4, lease_until = now(), lease_owner = NULL
		FROM ranked WHERE o.event_id = ranked.event_id`, batch, uuid.New(), uuid.New(), attempts)
	if err != nil {
		return fmt.Errorf("split outbox batch %s: %w", batch, err)
	}
	return nil
}

// SetAsideOutbox stops copying an event ClickHouse refuses on its own. It
// stays in the outbox, and in Postgres reports, until someone retries it.
func (r *Repository) SetAsideOutbox(ctx context.Context, event uuid.UUID, reason string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE clickhouse_outbox SET batch_id = NULL, lease_until = NULL, lease_owner = NULL, failed_at = now(), failure = $2
		WHERE event_id = $1`, event, reason)
	if err != nil {
		return fmt.Errorf("set aside outbox event %s: %w", event, err)
	}
	return nil
}

// OutboxBacklog reports the events waiting for ClickHouse, the age of the
// oldest of them, and the events set aside.
func (r *Repository) OutboxBacklog(ctx context.Context) (waiting int64, oldest time.Duration, setAside int64, err error) {
	var since *time.Time
	var now time.Time
	err = r.pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE failed_at IS NULL), min(enqueued_at) FILTER (WHERE failed_at IS NULL),
			count(*) FILTER (WHERE failed_at IS NOT NULL), now()
		FROM clickhouse_outbox`).Scan(&waiting, &since, &setAside, &now)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("outbox backlog: %w", err)
	}
	if since != nil {
		oldest = now.Sub(*since)
	}
	return waiting, oldest, setAside, nil
}

const eventColumns = `event_id, created_at, occurred_at, user_id, project_id, action_id,
	vendor, model, status, duration_ms, tokens_input, tokens_output, tokens_total,
	COALESCE(error_class, ''), COALESCE(vendor_http_status, 0), meta, cost_nanousd`

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
			&e.ErrorClass, &e.VendorHTTPStatus, &meta, &e.CostNanoUSD); err != nil {
			return nil, fmt.Errorf("scan outbox event: %w", err)
		}
		e.Status = domain.Status(status)
		if len(meta) > 0 {
			if err := json.Unmarshal(meta, &e.Meta); err != nil {
				return nil, fmt.Errorf("decode meta of %s: %w", e.EventID, err)
			}
		}
		events = append(events, &e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load outbox events: %w", err)
	}
	return events, nil
}
