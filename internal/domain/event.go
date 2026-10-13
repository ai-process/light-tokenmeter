package domain

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

// Status represents the outcome of an LLM request
type Status int

const (
	StatusUnspecified Status = 0
	StatusSuccess     Status = 1
	StatusError       Status = 2
	StatusCancelled   Status = 3
	StatusTimeout     Status = 4
)

func (s Status) String() string {
	switch s {
	case StatusSuccess:
		return "success"
	case StatusError:
		return "error"
	case StatusCancelled:
		return "cancelled"
	case StatusTimeout:
		return "timeout"
	default:
		return "unspecified"
	}
}

// StatusFromString converts a string to Status
func StatusFromString(s string) Status {
	switch s {
	case "success":
		return StatusSuccess
	case "error":
		return StatusError
	case "cancelled":
		return StatusCancelled
	case "timeout":
		return StatusTimeout
	default:
		return StatusUnspecified
	}
}

// UsageEvent represents a single LLM usage event
type UsageEvent struct {
	EventID          uuid.UUID
	CreatedAt        time.Time
	OccurredAt       *time.Time
	UserID           string
	ProjectID        string
	ActionID         string
	Vendor           string
	Model            string
	Status           Status
	DurationMs       int64
	TokensInput      int64
	TokensOutput     int64
	TokensTotal      int64
	ErrorClass       string
	VendorHTTPStatus int32
	Meta             map[string]string
}

// Validation errors
var (
	ErrInvalidEventID   = errors.New("invalid event_id: must be a valid UUID")
	ErrEmptyUserID      = errors.New("user_id is required")
	ErrEmptyProjectID   = errors.New("project_id is required")
	ErrEmptyActionID    = errors.New("action_id is required")
	ErrEmptyVendor      = errors.New("vendor is required")
	ErrEmptyModel       = errors.New("model is required")
	ErrInvalidStatus    = errors.New("status must be one of: success, error, cancelled, timeout")
	ErrUserIDTooLong    = errors.New("user_id exceeds 256 characters")
	ErrProjectIDTooLong = errors.New("project_id exceeds 256 characters")
	ErrActionIDTooLong  = errors.New("action_id exceeds 256 characters")
	ErrVendorTooLong    = errors.New("vendor exceeds 64 characters")
	ErrModelTooLong     = errors.New("model exceeds 128 characters")
	ErrTokensMismatch   = errors.New("tokens_total must equal tokens_input + tokens_output")
	ErrBatchTooLarge    = errors.New("batch size exceeds maximum of 500 events")
)

const (
	MaxStringLength = 256
	MaxVendorLength = 64
	MaxModelLength  = 128
	MaxBatchSize    = 500
)

// Validate validates the usage event
func (e *UsageEvent) Validate() error {
	if e.EventID == uuid.Nil {
		return ErrInvalidEventID
	}
	if e.UserID == "" {
		return ErrEmptyUserID
	}
	if len(e.UserID) > MaxStringLength {
		return ErrUserIDTooLong
	}
	if e.ProjectID == "" {
		return ErrEmptyProjectID
	}
	if len(e.ProjectID) > MaxStringLength {
		return ErrProjectIDTooLong
	}
	if e.ActionID == "" {
		return ErrEmptyActionID
	}
	if len(e.ActionID) > MaxStringLength {
		return ErrActionIDTooLong
	}
	if e.Vendor == "" {
		return ErrEmptyVendor
	}
	if len(e.Vendor) > MaxVendorLength {
		return ErrVendorTooLong
	}
	if e.Model == "" {
		return ErrEmptyModel
	}
	if len(e.Model) > MaxModelLength {
		return ErrModelTooLong
	}
	if e.Status == StatusUnspecified {
		return ErrInvalidStatus
	}

	// Validate or compute tokens_total
	expectedTotal := e.TokensInput + e.TokensOutput
	if e.TokensTotal == 0 {
		e.TokensTotal = expectedTotal
	} else if e.TokensTotal != expectedTotal {
		return ErrTokensMismatch
	}

	return nil
}

// MetaJSON returns the meta as JSON bytes
func (e *UsageEvent) MetaJSON() ([]byte, error) {
	if e.Meta == nil || len(e.Meta) == 0 {
		return nil, nil
	}
	return json.Marshal(e.Meta)
}
