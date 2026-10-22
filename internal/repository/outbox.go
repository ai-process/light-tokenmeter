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

// ClaimOutbox hands out the next batch for ClickHouse. A batch claimed more
// than staleAfter ago and never acknowledged (its replica died or is still
// retrying) comes first and keeps its ID, so ClickHouse drops a repeat.
func (r *Repository) ClaimOutbox(ctx context.Context, limit int, staleAfter time.Duration) (uuid.UUID, []*domain.UsageEvent, error) {
	ids, batch, err := r.claimStale(ctx, staleAfter)
	if err != nil {
		return uuid.Nil, nil, err
	}
	if len(ids) == 0 {
		batch = uuid.New()
		rows, err := r.pool.Query(ctx, `
			UPDATE clickhouse_outbox SET batch_id = $1, claimed_at = now()
			WHERE event_id IN (
				SELECT event_id FROM clickhouse_outbox WHERE batch_id IS NULL
				ORDER BY enqueued_at LIMIT $2 FOR UPDATE SKIP LOCKED)
			RETURNING event_id`, batch, limit)
		if err != nil {
			return uuid.Nil, nil, fmt.Errorf("claim outbox batch: %w", err)
		}
		if ids, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID]); err != nil {
			return uuid.Nil, nil, fmt.Errorf("claim outbox batch: %w", err)
		}
		if len(ids) == 0 {
			return uuid.Nil, nil, nil
		}
	}
	events, err := r.eventsByID(ctx, ids)
	if err != nil {
		return uuid.Nil, nil, err
	}
	return batch, events, nil
}

func (r *Repository) claimStale(ctx context.Context, staleAfter time.Duration) ([]uuid.UUID, uuid.UUID, error) {
	rows, err := r.pool.Query(ctx, `
		UPDATE clickhouse_outbox SET claimed_at = now()
		WHERE batch_id = (
			SELECT batch_id FROM clickhouse_outbox
			WHERE batch_id IS NOT NULL AND claimed_at < now() - make_interval(secs => $1)
			LIMIT 1 FOR UPDATE SKIP LOCKED)
		RETURNING event_id, batch_id`, staleAfter.Seconds())
	if err != nil {
		return nil, uuid.Nil, fmt.Errorf("reclaim stale outbox batch: %w", err)
	}
	defer rows.Close()
	var (
		ids   []uuid.UUID
		batch uuid.UUID
	)
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id, &batch); err != nil {
			return nil, uuid.Nil, fmt.Errorf("reclaim stale outbox batch: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, uuid.Nil, fmt.Errorf("reclaim stale outbox batch: %w", err)
	}
	return ids, batch, nil
}

// AckOutbox forgets a batch ClickHouse has stored.
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

func (r *Repository) eventsByID(ctx context.Context, ids []uuid.UUID) ([]*domain.UsageEvent, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT event_id, created_at, occurred_at, user_id, project_id, action_id,
			vendor, model, status, duration_ms, tokens_input, tokens_output, tokens_total,
			COALESCE(error_class, ''), COALESCE(vendor_http_status, 0), meta, cost_nanousd
		FROM usage_events WHERE event_id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("load outbox events: %w", err)
	}
	defer rows.Close()
	events := make([]*domain.UsageEvent, 0, len(ids))
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
