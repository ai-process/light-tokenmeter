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
