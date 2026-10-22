package domain

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestOccurredAtMustFitClickHouse(t *testing.T) {
	for at, want := range map[time.Time]error{
		{}: ErrOccurredAtRange, // Go's zero time, 0001-01-01
		time.Date(1899, 12, 31, 23, 59, 59, 0, time.UTC): ErrOccurredAtRange,
		time.Date(2300, 1, 1, 0, 0, 0, 0, time.UTC):      ErrOccurredAtRange,
		time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC):      nil,
		time.Now(): nil,
	} {
		e := UsageEvent{EventID: uuid.New(), UserID: "u", ProjectID: "p", ActionID: "a", Vendor: "v", Model: "m",
			Status: StatusSuccess, OccurredAt: &at}
		if err := e.Validate(); err != want {
			t.Errorf("occurred_at %v: %v, want %v", at, err, want)
		}
	}
}
