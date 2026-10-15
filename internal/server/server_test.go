package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "github.com/ai-process/light-tokenmeter/gen/usage/v1"
)

func TestHTTPServer_Validation(t *testing.T) {
	httpServer := NewHTTPServer(nil, ":0")

	t.Run("vendor too long returns 400", func(t *testing.T) {
		body, _ := json.Marshal(recordEventRequest{
			EventID:      uuid.NewString(),
			UserID:       "u1",
			ProjectID:    "p1",
			ActionID:     "a1",
			Vendor:       string(make([]byte, 65)),
			Model:        "gpt-4o",
			Status:       "success",
			TokensInput:  10,
			TokensOutput: 20,
		})

		req := httptest.NewRequest(http.MethodPost, "/v1/usage/events", bytes.NewReader(body))
		w := httptest.NewRecorder()
		httpServer.server.Handler.ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)
		assert.Contains(t, w.Body.String(), "vendor exceeds 64 characters")
	})

	t.Run("model too long returns 400", func(t *testing.T) {
		body, _ := json.Marshal(recordEventRequest{
			EventID:      uuid.NewString(),
			UserID:       "u1",
			ProjectID:    "p1",
			ActionID:     "a1",
			Vendor:       "openai",
			Model:        string(make([]byte, 129)),
			Status:       "success",
			TokensInput:  10,
			TokensOutput: 20,
		})

		req := httptest.NewRequest(http.MethodPost, "/v1/usage/events", bytes.NewReader(body))
		w := httptest.NewRecorder()
		httpServer.server.Handler.ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)
		assert.Contains(t, w.Body.String(), "model exceeds 128 characters")
	})

	t.Run("negative cost returns 400", func(t *testing.T) {
		body, err := json.Marshal(recordEventRequest{
			EventID:     uuid.NewString(),
			UserID:      "u1",
			ProjectID:   "p1",
			ActionID:    "a1",
			Vendor:      "openai",
			Model:       "gpt-4o",
			Status:      "success",
			CostNanoUSD: -5,
		})
		require.NoError(t, err)

		req := httptest.NewRequest(http.MethodPost, "/v1/usage/events", bytes.NewReader(body))
		w := httptest.NewRecorder()
		httpServer.server.Handler.ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)
		assert.Contains(t, w.Body.String(), "cost_nanousd must not be negative")
	})

	t.Run("invalid status filter on summary returns 400", func(t *testing.T) {
		q := url.Values{}
		q.Set("from", time.Now().UTC().Format(time.RFC3339))
		q.Set("to", time.Now().UTC().Add(time.Hour).Format(time.RFC3339))
		q.Set("status", "invalid_status")

		req := httptest.NewRequest(http.MethodGet, "/v1/usage/summary?"+q.Encode(), nil)
		w := httptest.NewRecorder()
		httpServer.server.Handler.ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)
		assert.Contains(t, w.Body.String(), "Invalid 'status' parameter")
	})

	t.Run("missing from/to returns 400", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/usage/summary", nil)
		w := httptest.NewRecorder()
		httpServer.server.Handler.ServeHTTP(w, req)

		assert.Equal(t, http.StatusBadRequest, w.Code)
		assert.Contains(t, w.Body.String(), "from and to query parameters are required")
	})
}

func TestProtoToEvent_Cost(t *testing.T) {
	ev, err := protoToEvent(&pb.UsageEvent{
		EventId:     uuid.NewString(),
		UserId:      "u1",
		ProjectId:   "p1",
		ActionId:    "a1",
		Vendor:      "openai",
		Model:       "gpt-4o",
		Status:      pb.Status_STATUS_SUCCESS,
		CostNanousd: 2_500_000,
	})
	require.NoError(t, err)
	assert.Equal(t, int64(2_500_000), ev.CostNanoUSD)

	_, err = protoToEvent(&pb.UsageEvent{
		EventId:     uuid.NewString(),
		UserId:      "u1",
		ProjectId:   "p1",
		ActionId:    "a1",
		Vendor:      "openai",
		Model:       "gpt-4o",
		Status:      pb.Status_STATUS_SUCCESS,
		CostNanousd: -1,
	})
	assert.ErrorContains(t, err, "cost_nanousd must not be negative")
}
