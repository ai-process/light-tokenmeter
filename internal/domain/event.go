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
	CostNanoUSD      int64
	// Prompt-cache tokens, both part of TokensInput: read from the vendor's
	// cache, and written to it, each priced differently from plain input.
	CacheReadTokens  int64
	CacheWriteTokens int64
	// A tool call through an MCP gateway; empty and 0 for an LLM call.
	ToolServer    string
	ToolName      string
	ResponseBytes int64
}

// Validation errors
var (
	ErrInvalidEventID    = errors.New("invalid event_id: must be a valid UUID")
	ErrEmptyUserID       = errors.New("user_id is required")
	ErrEmptyProjectID    = errors.New("project_id is required")
	ErrEmptyActionID     = errors.New("action_id is required")
	ErrEmptyVendor       = errors.New("vendor is required")
	ErrEmptyModel        = errors.New("model is required")
	ErrInvalidStatus     = errors.New("status must be one of: success, error, cancelled, timeout")
	ErrUserIDTooLong     = errors.New("user_id exceeds 256 characters")
	ErrProjectIDTooLong  = errors.New("project_id exceeds 256 characters")
	ErrActionIDTooLong   = errors.New("action_id exceeds 256 characters")
	ErrVendorTooLong     = errors.New("vendor exceeds 64 characters")
	ErrModelTooLong      = errors.New("model exceeds 128 characters")
	ErrTokensMismatch    = errors.New("tokens_total must equal tokens_input + tokens_output")
	ErrCacheTokens       = errors.New("cache_read_tokens and cache_write_tokens must be >= 0 and together not exceed tokens_input")
	ErrNegativeCost      = errors.New("cost_nanousd must not be negative")
	ErrToolCall          = errors.New("tool_call: server up to 64 and tool up to 128 characters, both set, response_bytes >= 0")
	ErrOccurredAtRange   = errors.New("occurred_at must be between 1900-01-01 and 2262-04-10")
	ErrErrorClassTooLong = errors.New("error_class exceeds 256 characters")
	ErrMetaTooLarge      = errors.New("meta exceeds 16384 bytes of keys and values")
	ErrMetaEmptyKey      = errors.New("meta keys must not be empty")
	ErrBatchTooLarge     = errors.New("batch size exceeds maximum of 500 events")
)

const (
	MaxStringLength = 256
	MaxVendorLength = 64
	MaxModelLength  = 128
	MaxBatchSize    = 500
	MaxMetaBytes    = 16 << 10
	// MaxRequestBytes bounds one request: a full batch of events at their limits.
	MaxRequestBytes = 16 << 20
)

var (
	minOccurredAt = time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC)
	maxOccurredAt = time.Date(2262, 4, 11, 0, 0, 0, 0, time.UTC) // nanoseconds since 1970 fit int64
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
	if (e.ToolServer != "" || e.ToolName != "" || e.ResponseBytes != 0) &&
		(e.ToolServer == "" || e.ToolName == "" || len(e.ToolServer) > MaxVendorLength || len(e.ToolName) > MaxModelLength || e.ResponseBytes < 0) {
		return ErrToolCall
	}
	if e.CostNanoUSD < 0 {
		return ErrNegativeCost
	}
	// Checked only when sent, so events without them validate as before; the
	// subtraction cannot overflow once 0 <= read <= input.
	if (e.CacheReadTokens != 0 || e.CacheWriteTokens != 0) && (e.CacheReadTokens < 0 || e.CacheWriteTokens < 0 ||
		e.CacheReadTokens > e.TokensInput || e.CacheWriteTokens > e.TokensInput-e.CacheReadTokens) {
		return ErrCacheTokens
	}
	if len(e.ErrorClass) > MaxStringLength {
		return ErrErrorClassTooLong
	}
	metaBytes := 0
	for k, v := range e.Meta {
		if k == "" {
			return ErrMetaEmptyKey
		}
		metaBytes += len(k) + len(v)
	}
	if metaBytes > MaxMetaBytes {
		return ErrMetaTooLarge
	}
	// Go's zero time is what clients send for "unset"; anything else must fit
	// what the ClickHouse driver can encode, or it would be corrupted there.
	if e.OccurredAt != nil && e.OccurredAt.IsZero() {
		e.OccurredAt = nil
	}
	if e.OccurredAt != nil && (e.OccurredAt.Before(minOccurredAt) || !e.OccurredAt.Before(maxOccurredAt)) {
		return ErrOccurredAtRange
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
