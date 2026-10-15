package clickhouse

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"

	"github.com/ai-process/light-tokenmeter/internal/domain"
)

const defaultMaxQueueSize = 50000

// BatchWriter buffers and deduplicates events in memory before batch flushing to ClickHouse.
type BatchWriter struct {
	conn          driver.Conn
	batchSize     int
	flushInterval time.Duration
	maxQueueSize  int

	queue   chan *domain.UsageEvent
	flushCh chan chan error
	wg      sync.WaitGroup
	closed  atomic.Bool
}

// NewBatchWriter constructs an asynchronous batch writer.
func NewBatchWriter(conn driver.Conn, batchSize int, flushInterval time.Duration) *BatchWriter {
	if batchSize <= 0 {
		batchSize = 5000
	}
	if flushInterval <= 0 {
		flushInterval = time.Second
	}

	return &BatchWriter{
		conn:          conn,
		batchSize:     batchSize,
		flushInterval: flushInterval,
		maxQueueSize:  defaultMaxQueueSize,
		queue:         make(chan *domain.UsageEvent, defaultMaxQueueSize),
		flushCh:       make(chan chan error),
	}
}

// Start launches the background worker goroutine.
func (w *BatchWriter) Start() {
	w.wg.Add(1)
	go w.run()
}

// Enqueue adds events to the in-memory queue with non-blocking backpressure.
func (w *BatchWriter) Enqueue(events ...*domain.UsageEvent) {
	if w.closed.Load() {
		return
	}
	for _, ev := range events {
		if ev == nil {
			continue
		}
		select {
		case w.queue <- ev:
		default:
			log.Warn().
				Str("event_id", ev.EventID.String()).
				Msg("clickhouse batch buffer full, event dropped from async sink")
		}
	}
}

// Flush triggers an immediate synchronous flush of pending events.
func (w *BatchWriter) Flush(ctx context.Context) error {
	if w.closed.Load() {
		return nil
	}
	errCh := make(chan error, 1)
	select {
	case w.flushCh <- errCh:
		select {
		case err := <-errCh:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close stops the writer, flushes all remaining queued items, and waits for completion.
func (w *BatchWriter) Close() error {
	if !w.closed.CompareAndSwap(false, true) {
		return nil
	}
	close(w.queue)
	w.wg.Wait()
	return nil
}

func (w *BatchWriter) run() {
	defer w.wg.Done()

	ticker := time.NewTicker(w.flushInterval)
	defer ticker.Stop()

	batch := make([]*domain.UsageEvent, 0, w.batchSize)
	seen := make(map[uuid.UUID]struct{}, w.batchSize)

	doFlush := func() error {
		if len(batch) == 0 {
			return nil
		}
		err := w.sendBatch(context.Background(), batch)
		batch = make([]*domain.UsageEvent, 0, w.batchSize)
		clear(seen)
		return err
	}

	for {
		select {
		case ev, ok := <-w.queue:
			if !ok {
				if err := doFlush(); err != nil {
					log.Error().Err(err).Msg("error flushing final batch to clickhouse on shutdown")
				}
				return
			}
			if _, exists := seen[ev.EventID]; exists {
				continue
			}
			seen[ev.EventID] = struct{}{}
			batch = append(batch, ev)
			if len(batch) >= w.batchSize {
				if err := doFlush(); err != nil {
					log.Error().Err(err).Msg("error flushing batch to clickhouse")
				}
			}

		case <-ticker.C:
			if len(batch) > 0 {
				if err := doFlush(); err != nil {
					log.Error().Err(err).Msg("error flushing batch to clickhouse on interval")
				}
			}

		case errCh := <-w.flushCh:
			for {
				select {
				case ev, ok := <-w.queue:
					if !ok {
						break
					}
					if _, exists := seen[ev.EventID]; !exists {
						seen[ev.EventID] = struct{}{}
						batch = append(batch, ev)
					}
				default:
					goto drained
				}
			}
		drained:
			err := doFlush()
			errCh <- err
		}
	}
}

func (w *BatchWriter) sendBatch(ctx context.Context, events []*domain.UsageEvent) error {
	if len(events) == 0 {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	batch, err := w.conn.PrepareBatch(ctx, `
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

		err := batch.Append(
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

	if err := batch.Send(); err != nil {
		return fmt.Errorf("send batch to clickhouse: %w", err)
	}

	log.Debug().Int("events", len(events)).Msg("successfully flushed batch to clickhouse")
	return nil
}
