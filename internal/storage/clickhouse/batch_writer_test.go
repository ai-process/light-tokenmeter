package clickhouse

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ai-process/light-tokenmeter/internal/domain"
)

type mockBatch struct {
	driver.Batch
	rowsMu sync.Mutex
	rows   [][]any
	sent   bool
}

func (m *mockBatch) Append(v ...any) error {
	m.rowsMu.Lock()
	defer m.rowsMu.Unlock()
	m.rows = append(m.rows, v)
	return nil
}

func (m *mockBatch) Send() error {
	m.rowsMu.Lock()
	defer m.rowsMu.Unlock()
	m.sent = true
	return nil
}

type mockConn struct {
	driver.Conn
	mu      sync.Mutex
	batches []*mockBatch
}

func (m *mockConn) PrepareBatch(ctx context.Context, query string, opts ...driver.PrepareBatchOption) (driver.Batch, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := &mockBatch{}
	m.batches = append(m.batches, b)
	return b, nil
}

func (m *mockConn) totalRows() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	count := 0
	for _, b := range m.batches {
		b.rowsMu.Lock()
		count += len(b.rows)
		b.rowsMu.Unlock()
	}
	return count
}

func TestBatchWriter_DeduplicationAndFlush(t *testing.T) {
	conn := &mockConn{}
	writer := NewBatchWriter(conn, 100, 50*time.Millisecond)
	writer.Start()

	u1 := uuid.New()
	u2 := uuid.New()
	u3 := uuid.New()

	ev1 := &domain.UsageEvent{EventID: u1, ProjectID: "p1", UserID: "u1", Vendor: "openai", Model: "gpt-4o", Status: domain.StatusSuccess}
	ev1Dup := &domain.UsageEvent{EventID: u1, ProjectID: "p1", UserID: "u1", Vendor: "openai", Model: "gpt-4o", Status: domain.StatusSuccess}
	ev2 := &domain.UsageEvent{EventID: u2, ProjectID: "p1", UserID: "u1", Vendor: "openai", Model: "gpt-4o", Status: domain.StatusSuccess}
	ev3 := &domain.UsageEvent{EventID: u3, ProjectID: "p1", UserID: "u1", Vendor: "openai", Model: "gpt-4o", Status: domain.StatusSuccess}

	// Enqueue events with duplicate
	writer.Enqueue(ev1, ev1Dup, ev2, ev3)

	// Explicit synchronous flush
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := writer.Flush(ctx)
	require.NoError(t, err)

	// Verify only 3 unique events written (ev1 was deduped)
	assert.Equal(t, 3, conn.totalRows())

	err = writer.Close()
	require.NoError(t, err)
}

func TestBatchWriter_PeriodicFlush(t *testing.T) {
	conn := &mockConn{}
	writer := NewBatchWriter(conn, 100, 20*time.Millisecond)
	writer.Start()

	writer.Enqueue(&domain.UsageEvent{
		EventID:   uuid.New(),
		ProjectID: "p1",
		UserID:    "u1",
		Vendor:    "openai",
		Model:     "gpt-4o",
		Status:    domain.StatusSuccess,
	})

	// Wait for ticker flush
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, 1, conn.totalRows())

	err := writer.Close()
	require.NoError(t, err)
}

func TestBatchWriter_ShutdownDrainsQueue(t *testing.T) {
	conn := &mockConn{}
	writer := NewBatchWriter(conn, 1000, 10*time.Second) // Long flush interval so only shutdown triggers it
	writer.Start()

	for i := 0; i < 50; i++ {
		writer.Enqueue(&domain.UsageEvent{
			EventID:   uuid.New(),
			ProjectID: "p1",
			UserID:    "u1",
			Vendor:    "openai",
			Model:     "gpt-4o",
			Status:    domain.StatusSuccess,
		})
	}

	err := writer.Close()
	require.NoError(t, err)

	// All 50 events must have been flushed before Close returned
	assert.Equal(t, 50, conn.totalRows())
}

func TestBatchWriter_ConcurrentEnqueue(t *testing.T) {
	conn := &mockConn{}
	writer := NewBatchWriter(conn, 50, 10*time.Millisecond)
	writer.Start()

	var wg sync.WaitGroup
	const goroutines = 20
	const perGoroutine = 50

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				writer.Enqueue(&domain.UsageEvent{
					EventID:   uuid.New(),
					ProjectID: "p1",
					UserID:    "u1",
					Vendor:    "openai",
					Model:     "gpt-4o",
					Status:    domain.StatusSuccess,
				})
			}
		}()
	}

	wg.Wait()
	err := writer.Close()
	require.NoError(t, err)

	assert.Equal(t, goroutines*perGoroutine, conn.totalRows())
}
