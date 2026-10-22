package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ai-process/light-tokenmeter/internal/domain"
)

// ClaimOutbox hands out the next batch for ClickHouse. A batch claimed more
// than staleAfter ago and never acknowledged (its replica died or gave up)
// comes first and keeps its ID, so ClickHouse drops a repeat.
func (r *Repository) ClaimOutbox(ctx context.Context, limit int, staleAfter time.Duration) (uuid.UUID, []*domain.UsageEvent, error) {
	batch, ids, err := r.claimStale(ctx, staleAfter)
	if err != nil {
		return uuid.Nil, nil, err
	}
	if batch != uuid.Nil {
		events, err := r.eventsByID(ctx, ids)
		return batch, events, err
	}
	batch = uuid.New()
	// SKIP LOCKED lets replicas claim side by side; a row another claim just
	// took fails the batch_id IS NULL recheck and is skipped.
	rows, err := r.pool.Query(ctx, `
		WITH claimed AS (
			UPDATE clickhouse_outbox SET batch_id = $1, claimed_at = now()
			WHERE event_id IN (
				SELECT event_id FROM clickhouse_outbox WHERE batch_id IS NULL
				ORDER BY enqueued_at LIMIT $2 FOR UPDATE SKIP LOCKED)
			RETURNING event_id)
		SELECT `+eventColumns+` FROM usage_events JOIN claimed USING (event_id)`, batch, limit)
	if err != nil {
		return uuid.Nil, nil, fmt.Errorf("claim outbox batch: %w", err)
	}
	events, err := scanEvents(rows, limit)
	if err != nil || len(events) == 0 {
		return uuid.Nil, nil, err
	}
	return batch, events, nil
}

// claimStale takes over one stale batch. The advisory lock makes a second
// replica wait and then find the batch fresh, instead of copying it too.
func (r *Repository) claimStale(ctx context.Context, staleAfter time.Duration) (uuid.UUID, []uuid.UUID, error) {
	var batch uuid.UUID
	err := r.pool.QueryRow(ctx, `
		SELECT batch_id FROM clickhouse_outbox
		WHERE batch_id IS NOT NULL AND claimed_at < now() - make_interval(secs => $1)
		LIMIT 1`, staleAfter.Seconds()).Scan(&batch)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, nil, nil
	}
	if err != nil {
		return uuid.Nil, nil, fmt.Errorf("find stale outbox batch: %w", err)
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, nil, fmt.Errorf("reclaim stale outbox batch: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text, 0))`, batch); err != nil {
		return uuid.Nil, nil, fmt.Errorf("reclaim stale outbox batch: %w", err)
	}
	rows, err := tx.Query(ctx, `
		UPDATE clickhouse_outbox SET claimed_at = now()
		WHERE batch_id = $1 AND claimed_at < now() - make_interval(secs => $2)
		RETURNING event_id`, batch, staleAfter.Seconds())
	if err != nil {
		return uuid.Nil, nil, fmt.Errorf("reclaim stale outbox batch: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return uuid.Nil, nil, fmt.Errorf("reclaim stale outbox batch: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, nil, fmt.Errorf("reclaim stale outbox batch: %w", err)
	}
	if len(ids) == 0 {
		return uuid.Nil, nil, nil // another replica took it first
	}
	return batch, ids, nil
}

func (r *Repository) AckOutbox(ctx context.Context, batch uuid.UUID) error {
	if _, err := r.pool.Exec(ctx, `DELETE FROM clickhouse_outbox WHERE batch_id = $1`, batch); err != nil {
		return fmt.Errorf("ack outbox batch %s: %w", batch, err)
	}
	return nil
}

// OutboxBacklog reports how many events wait for ClickHouse and the age of
// the oldest, zero when none wait.
func (r *Repository) OutboxBacklog(ctx context.Context) (int64, time.Duration, error) {
	var (
		n      int64
		oldest *time.Time
		now    time.Time
	)
	err := r.pool.QueryRow(ctx, `SELECT count(*), min(enqueued_at), now() FROM clickhouse_outbox`).Scan(&n, &oldest, &now)
	if err != nil {
		return 0, 0, fmt.Errorf("outbox backlog: %w", err)
	}
	if oldest == nil {
		return 0, 0, nil
	}
	return n, now.Sub(*oldest), nil
}

const eventColumns = `event_id, created_at, occurred_at, user_id, project_id, action_id,
	vendor, model, status, duration_ms, tokens_input, tokens_output, tokens_total,
	COALESCE(error_class, ''), COALESCE(vendor_http_status, 0), meta, cost_nanousd`

func (r *Repository) eventsByID(ctx context.Context, ids []uuid.UUID) ([]*domain.UsageEvent, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+eventColumns+` FROM usage_events WHERE event_id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("load outbox events: %w", err)
	}
	return scanEvents(rows, len(ids))
}

func scanEvents(rows pgx.Rows, capacity int) ([]*domain.UsageEvent, error) {
	defer rows.Close()
	events := make([]*domain.UsageEvent, 0, capacity)
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
