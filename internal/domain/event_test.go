package domain

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestOccurredAtMustFitClickHouse(t *testing.T) {
	for at, want := range map[time.Time]error{
		time.Date(1899, 12, 31, 23, 59, 59, 0, time.UTC): ErrOccurredAtRange,
		time.Date(2262, 4, 11, 0, 0, 0, 0, time.UTC):     ErrOccurredAtRange,
		time.Date(2262, 4, 10, 23, 0, 0, 0, time.UTC):    nil,
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

func TestZeroOccurredAtMeansUnset(t *testing.T) {
	var zero time.Time
	e := UsageEvent{EventID: uuid.New(), UserID: "u", ProjectID: "p", ActionID: "a", Vendor: "v", Model: "m",
		Status: StatusSuccess, OccurredAt: &zero}
	if err := e.Validate(); err != nil || e.OccurredAt != nil {
		t.Fatalf("zero occurred_at: %v, %v; want it treated as unset", err, e.OccurredAt)
	}
}

func TestLimitsOnErrorClassAndMeta(t *testing.T) {
	base := func() UsageEvent {
		return UsageEvent{EventID: uuid.New(), UserID: "u", ProjectID: "p", ActionID: "a", Vendor: "v", Model: "m", Status: StatusSuccess}
	}
	e := base()
	e.ErrorClass = strings.Repeat("x", MaxStringLength+1)
	if err := e.Validate(); err != ErrErrorClassTooLong {
		t.Errorf("long error_class: %v", err)
	}
	e = base()
	e.Meta = map[string]string{"k": strings.Repeat("v", MaxMetaBytes)}
	if err := e.Validate(); err != ErrMetaTooLarge {
		t.Errorf("large meta: %v", err)
	}
	e = base()
	e.Meta = map[string]string{"": "v"}
	if err := e.Validate(); err != ErrMetaEmptyKey {
		t.Errorf("empty meta key: %v", err)
	}
	e = base()
	e.Meta = map[string]string{"k": strings.Repeat("v", MaxMetaBytes-1)}
	if err := e.Validate(); err != nil {
		t.Errorf("meta at the limit: %v", err)
	}
}
