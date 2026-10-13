package repository

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ai-process/light-tokenmeter/internal/domain"
)

func TestValidateEvent(t *testing.T) {
	tests := []struct {
		name    string
		event   domain.UsageEvent
		wantErr error
	}{
		{
			name: "valid event",
			event: domain.UsageEvent{
				EventID:      uuid.New(),
				UserID:       "user-123",
				ProjectID:    "project-456",
				ActionID:     "action-789",
				Vendor:       "openai",
				Model:        "gpt-4o-mini",
				Status:       domain.StatusSuccess,
				DurationMs:   100,
				TokensInput:  50,
				TokensOutput: 100,
			},
			wantErr: nil,
		},
		{
			name: "valid event with tokens_total",
			event: domain.UsageEvent{
				EventID:      uuid.New(),
				UserID:       "user-123",
				ProjectID:    "project-456",
				ActionID:     "action-789",
				Vendor:       "openai",
				Model:        "gpt-4o-mini",
				Status:       domain.StatusSuccess,
				TokensInput:  50,
				TokensOutput: 100,
				TokensTotal:  150,
			},
			wantErr: nil,
		},
		{
			name: "invalid tokens_total",
			event: domain.UsageEvent{
				EventID:      uuid.New(),
				UserID:       "user-123",
				ProjectID:    "project-456",
				ActionID:     "action-789",
				Vendor:       "openai",
				Model:        "gpt-4o-mini",
				Status:       domain.StatusSuccess,
				TokensInput:  50,
				TokensOutput: 100,
				TokensTotal:  200, // Should be 150
			},
			wantErr: domain.ErrTokensMismatch,
		},
		{
			name: "empty event_id",
			event: domain.UsageEvent{
				UserID:    "user-123",
				ProjectID: "project-456",
				ActionID:  "action-789",
				Vendor:    "openai",
				Model:     "gpt-4o-mini",
				Status:    domain.StatusSuccess,
			},
			wantErr: domain.ErrInvalidEventID,
		},
		{
			name: "empty user_id",
			event: domain.UsageEvent{
				EventID:   uuid.New(),
				ProjectID: "project-456",
				ActionID:  "action-789",
				Vendor:    "openai",
				Model:     "gpt-4o-mini",
				Status:    domain.StatusSuccess,
			},
			wantErr: domain.ErrEmptyUserID,
		},
		{
			name: "empty project_id",
			event: domain.UsageEvent{
				EventID:  uuid.New(),
				UserID:   "user-123",
				ActionID: "action-789",
				Vendor:   "openai",
				Model:    "gpt-4o-mini",
				Status:   domain.StatusSuccess,
			},
			wantErr: domain.ErrEmptyProjectID,
		},
		{
			name: "empty action_id",
			event: domain.UsageEvent{
				EventID:   uuid.New(),
				UserID:    "user-123",
				ProjectID: "project-456",
				Vendor:    "openai",
				Model:     "gpt-4o-mini",
				Status:    domain.StatusSuccess,
			},
			wantErr: domain.ErrEmptyActionID,
		},
		{
			name: "empty vendor",
			event: domain.UsageEvent{
				EventID:   uuid.New(),
				UserID:    "user-123",
				ProjectID: "project-456",
				ActionID:  "action-789",
				Model:     "gpt-4o-mini",
				Status:    domain.StatusSuccess,
			},
			wantErr: domain.ErrEmptyVendor,
		},
		{
			name: "empty model",
			event: domain.UsageEvent{
				EventID:   uuid.New(),
				UserID:    "user-123",
				ProjectID: "project-456",
				ActionID:  "action-789",
				Vendor:    "openai",
				Status:    domain.StatusSuccess,
			},
			wantErr: domain.ErrEmptyModel,
		},
		{
			name: "invalid status",
			event: domain.UsageEvent{
				EventID:   uuid.New(),
				UserID:    "user-123",
				ProjectID: "project-456",
				ActionID:  "action-789",
				Vendor:    "openai",
				Model:     "gpt-4o-mini",
			},
			wantErr: domain.ErrInvalidStatus,
		},
		{
			name: "user_id too long",
			event: domain.UsageEvent{
				EventID:   uuid.New(),
				UserID:    string(make([]byte, 257)),
				ProjectID: "project-456",
				ActionID:  "action-789",
				Vendor:    "openai",
				Model:     "gpt-4o-mini",
				Status:    domain.StatusSuccess,
			},
			wantErr: domain.ErrUserIDTooLong,
		},
		{
			name: "vendor too long",
			event: domain.UsageEvent{
				EventID:   uuid.New(),
				UserID:    "user-123",
				ProjectID: "project-456",
				ActionID:  "action-789",
				Vendor:    string(make([]byte, 65)),
				Model:     "gpt-4o-mini",
				Status:    domain.StatusSuccess,
			},
			wantErr: domain.ErrVendorTooLong,
		},
		{
			name: "model too long",
			event: domain.UsageEvent{
				EventID:   uuid.New(),
				UserID:    "user-123",
				ProjectID: "project-456",
				ActionID:  "action-789",
				Vendor:    "openai",
				Model:     string(make([]byte, 129)),
				Status:    domain.StatusSuccess,
			},
			wantErr: domain.ErrModelTooLong,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.event.Validate()
			if tt.wantErr != nil {
				assert.ErrorIs(t, err, tt.wantErr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestTokensTotalComputation(t *testing.T) {
	event := domain.UsageEvent{
		EventID:      uuid.New(),
		UserID:       "user-123",
		ProjectID:    "project-456",
		ActionID:     "action-789",
		Vendor:       "openai",
		Model:        "gpt-4o-mini",
		Status:       domain.StatusSuccess,
		TokensInput:  50,
		TokensOutput: 100,
		TokensTotal:  0, // Should be computed
	}

	err := event.Validate()
	require.NoError(t, err)
	assert.Equal(t, int64(150), event.TokensTotal)
}

func TestStatusString(t *testing.T) {
	tests := []struct {
		status   domain.Status
		expected string
	}{
		{domain.StatusSuccess, "success"},
		{domain.StatusError, "error"},
		{domain.StatusCancelled, "cancelled"},
		{domain.StatusTimeout, "timeout"},
		{domain.StatusUnspecified, "unspecified"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			assert.Equal(t, tt.expected, tt.status.String())
		})
	}
}

func TestStatusFromString(t *testing.T) {
	tests := []struct {
		input    string
		expected domain.Status
	}{
		{"success", domain.StatusSuccess},
		{"error", domain.StatusError},
		{"cancelled", domain.StatusCancelled},
		{"timeout", domain.StatusTimeout},
		{"unknown", domain.StatusUnspecified},
		{"", domain.StatusUnspecified},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			assert.Equal(t, tt.expected, domain.StatusFromString(tt.input))
		})
	}
}

func TestMetaJSON(t *testing.T) {
	t.Run("nil meta returns nil", func(t *testing.T) {
		event := domain.UsageEvent{}
		data, err := event.MetaJSON()
		require.NoError(t, err)
		assert.Nil(t, data)
	})

	t.Run("empty meta returns nil", func(t *testing.T) {
		event := domain.UsageEvent{Meta: map[string]string{}}
		data, err := event.MetaJSON()
		require.NoError(t, err)
		assert.Nil(t, data)
	})

	t.Run("meta with values returns JSON", func(t *testing.T) {
		event := domain.UsageEvent{
			Meta: map[string]string{
				"key1": "value1",
				"key2": "value2",
			},
		}
		data, err := event.MetaJSON()
		require.NoError(t, err)
		assert.Contains(t, string(data), "key1")
		assert.Contains(t, string(data), "value1")
	})
}

func TestSummaryParams(t *testing.T) {
	now := time.Now()
	params := domain.SummaryParams{
		From:             now.Add(-24 * time.Hour),
		To:               now,
		ProjectID:        "project-123",
		GroupByProjectID: true,
		GroupByVendor:    true,
	}

	assert.Equal(t, "project-123", params.ProjectID)
	assert.True(t, params.GroupByProjectID)
	assert.True(t, params.GroupByVendor)
	assert.False(t, params.GroupByUserID)
}
