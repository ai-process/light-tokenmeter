package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/ai-process/light-tokenmeter/internal/auth"
	"github.com/ai-process/light-tokenmeter/internal/domain"
	"github.com/ai-process/light-tokenmeter/internal/metrics"
	"github.com/ai-process/light-tokenmeter/internal/repository"
	"github.com/ai-process/light-tokenmeter/internal/service"
)

// HTTPServer wraps the HTTP server implementation
type HTTPServer struct {
	service *service.UsageService
	repo    *repository.Repository
	server  *http.Server
	addr    string
}

// NewHTTPServer creates a new HTTP server
func NewHTTPServer(repo *repository.Repository, addr string) *HTTPServer {
	return NewHTTPServerWithService(service.NewUsageService(repo), repo, addr, nil)
}

// NewHTTPServerWithService creates a new HTTP server with an existing usage
// service. A nil or empty verifier serves without authentication.
func NewHTTPServerWithService(usageService *service.UsageService, repo *repository.Repository, addr string, verifier *auth.Verifier) *HTTPServer {
	s := &HTTPServer{
		service: usageService,
		repo:    repo,
		addr:    addr,
	}

	mux := http.NewServeMux()

	// Health endpoints
	route := func(pattern string, h http.Handler) { mux.Handle(pattern, instrument(pattern, h)) }
	// Probes are not API traffic and stay out of the request metrics.
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)

	// Usage API endpoints
	require := func(scope auth.Scope, h http.HandlerFunc) http.Handler {
		return auth.Require(verifier, scope, func(w http.ResponseWriter, status int, code, message string) {
			writeError(w, status, code, message, "")
		}, h)
	}
	route("POST /v1/usage/events", require(auth.Write, s.handleRecordEvent))
	route("POST /v1/usage/events:batch", require(auth.Write, s.handleRecordEvents))
	route("GET /v1/usage/summary", require(auth.Read, s.handleGetSummary))

	s.server = &http.Server{
		Addr:         addr,
		Handler:      loggingMiddleware(mux),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	return s
}

// Start starts the HTTP server
func (s *HTTPServer) Start() error {
	log.Info().Str("addr", s.addr).Msg("starting HTTP server")
	return s.server.ListenAndServe()
}

// Shutdown gracefully shuts down the HTTP server
func (s *HTTPServer) Shutdown(ctx context.Context) error {
	log.Info().Msg("shutting down HTTP server")
	return s.server.Shutdown(ctx)
}

// Health check handlers
// Version is the build's release version, reported by /healthz.
var Version = "dev"

func (s *HTTPServer) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "version": Version})
}

func (s *HTTPServer) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	if s.repo != nil {
		if err := s.repo.Ping(ctx); err != nil {
			log.Warn().Err(err).Msg("readiness check failed: postgres unavailable")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"not ready","error":"database unavailable"}`))
			return
		}
	}

	// ClickHouse down is not unready: writes go to Postgres and its outbox,
	// and reads fall back to Postgres.
	body := `{"status":"ready"}`
	if s.service != nil && s.service.AnalyticsMissing() {
		body = `{"status":"ready","clickhouse":"unavailable"}`
	} else if s.service != nil && s.service.Analytics() != nil {
		if err := s.service.Analytics().Ping(ctx); err != nil {
			log.Warn().Err(err).Msg("clickhouse unavailable; serving reads from postgres")
			body = `{"status":"ready","clickhouse":"unavailable"}`
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(body))
}

// API request/response types
type recordEventRequest struct {
	EventID          string            `json:"event_id"`
	OccurredAt       *time.Time        `json:"occurred_at,omitempty"`
	UserID           string            `json:"user_id"`
	ProjectID        string            `json:"project_id"`
	ActionID         string            `json:"action_id"`
	Vendor           string            `json:"vendor"`
	Model            string            `json:"model"`
	Status           string            `json:"status"`
	DurationMs       int64             `json:"duration_ms"`
	TokensInput      int64             `json:"tokens_input"`
	TokensOutput     int64             `json:"tokens_output"`
	TokensTotal      int64             `json:"tokens_total,omitempty"`
	ErrorClass       string            `json:"error_class,omitempty"`
	VendorHTTPStatus int32             `json:"vendor_http_status,omitempty"`
	Meta             map[string]string `json:"meta,omitempty"`
	CostNanoUSD      int64             `json:"cost_nanousd,omitempty"`
	CacheReadTokens  int64             `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int64             `json:"cache_write_tokens,omitempty"`
	ToolCall         *toolCall         `json:"tool_call,omitempty"`
}

type toolCall struct {
	Server        string `json:"server"`
	Tool          string `json:"tool"`
	ResponseBytes int64  `json:"response_bytes,omitempty"`
}

type recordEventResponse struct {
	Created bool `json:"created"`
}

type recordEventsRequest struct {
	Events []recordEventRequest `json:"events"`
}

type recordEventsResponse struct {
	CreatedCount   int `json:"created_count"`
	DuplicateCount int `json:"duplicate_count"`
}

type getSummaryRequest struct {
	From             string `json:"from"`
	To               string `json:"to"`
	ProjectID        string `json:"project_id,omitempty"`
	UserID           string `json:"user_id,omitempty"`
	Vendor           string `json:"vendor,omitempty"`
	Model            string `json:"model,omitempty"`
	ActionID         string `json:"action_id,omitempty"`
	Status           string `json:"status,omitempty"`
	GroupByProjectID bool   `json:"group_by_project_id,omitempty"`
	GroupByUserID    bool   `json:"group_by_user_id,omitempty"`
	GroupByVendor    bool   `json:"group_by_vendor,omitempty"`
	GroupByModel     bool   `json:"group_by_model,omitempty"`
	GroupByActionID  bool   `json:"group_by_action_id,omitempty"`
	GroupByStatus    bool   `json:"group_by_status,omitempty"`
}

type summaryRow struct {
	Bucket        string `json:"bucket"`
	ProjectID     string `json:"project_id,omitempty"`
	UserID        string `json:"user_id,omitempty"`
	Vendor        string `json:"vendor,omitempty"`
	Model         string `json:"model,omitempty"`
	ActionID      string `json:"action_id,omitempty"`
	Status        string `json:"status,omitempty"`
	TokensInput   int64  `json:"tokens_input"`
	TokensOutput  int64  `json:"tokens_output"`
	TokensTotal   int64  `json:"tokens_total"`
	RequestCount  int64  `json:"request_count"`
	SumDurationMs int64  `json:"sum_duration_ms"`
	CostNanoUSD   int64  `json:"cost_nanousd"`
	// Parts of tokens_input read from and written to the prompt cache.
	CacheReadTokens  int64 `json:"cache_read_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
}

type getSummaryResponse struct {
	Rows      []summaryRow `json:"rows"`
	Truncated bool         `json:"truncated"`
}

type errorResponse struct {
	Error   string `json:"error"`
	Code    string `json:"code,omitempty"`
	Details string `json:"details,omitempty"`
}

func (s *HTTPServer) handleRecordEvent(w http.ResponseWriter, r *http.Request) {
	var req recordEventRequest
	if !decodeBody(w, r, &req) {
		return
	}

	event, err := httpRequestToEvent(&req)
	if err != nil {
		metrics.Events.WithLabelValues("invalid").Inc()
		writeError(w, http.StatusBadRequest, "validation_error", err.Error(), "")
		return
	}

	created, err := s.service.RecordEvent(r.Context(), event)
	if err != nil {
		log.Error().Err(err).Str("event_id", event.EventID.String()).Msg("failed to record event")
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to record event", "")
		return
	}

	writeJSON(w, http.StatusOK, recordEventResponse{Created: created})
}

func (s *HTTPServer) handleRecordEvents(w http.ResponseWriter, r *http.Request) {
	var req recordEventsRequest
	if !decodeBody(w, r, &req) {
		return
	}

	if len(req.Events) == 0 {
		writeError(w, http.StatusBadRequest, "validation_error", "Events list is empty", "")
		return
	}

	if len(req.Events) > domain.MaxBatchSize {
		metrics.Events.WithLabelValues("invalid").Add(float64(len(req.Events)))
		writeError(w, http.StatusBadRequest, "validation_error", fmt.Sprintf("Batch size exceeds maximum of %d events", domain.MaxBatchSize), "")
		return
	}

	events := make([]*domain.UsageEvent, 0, len(req.Events))
	for i, eventReq := range req.Events {
		event, err := httpRequestToEvent(&eventReq)
		if err != nil {
			metrics.Events.WithLabelValues("invalid").Add(float64(len(req.Events))) // the whole batch is refused
			writeError(w, http.StatusBadRequest, "validation_error", fmt.Sprintf("invalid event at index %d: %s", i, err.Error()), "")
			return
		}
		events = append(events, event)
	}

	created, duplicates, err := s.service.RecordEvents(r.Context(), events)
	if err != nil {
		log.Error().Err(err).Int("batch_size", len(events)).Msg("failed to record events batch")
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to record events", "")
		return
	}

	writeJSON(w, http.StatusOK, recordEventsResponse{
		CreatedCount:   created,
		DuplicateCount: duplicates,
	})
}

func (s *HTTPServer) handleGetSummary(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	fromStr := q.Get("from")
	toStr := q.Get("to")

	if fromStr == "" || toStr == "" {
		writeError(w, http.StatusBadRequest, "validation_error", "from and to query parameters are required", "")
		return
	}

	from, err := time.Parse(time.RFC3339, fromStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "validation_error", "Invalid 'from' timestamp format (expected RFC3339)", err.Error())
		return
	}

	to, err := time.Parse(time.RFC3339, toStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "validation_error", "Invalid 'to' timestamp format (expected RFC3339)", err.Error())
		return
	}

	status := domain.StatusUnspecified
	if statusStr := q.Get("status"); statusStr != "" {
		status = domain.StatusFromString(statusStr)
		if status == domain.StatusUnspecified {
			writeError(w, http.StatusBadRequest, "validation_error", "Invalid 'status' parameter (expected success, error, cancelled, or timeout)", "")
			return
		}
	}

	params := domain.SummaryParams{
		From:             from,
		To:               to,
		ProjectID:        q.Get("project_id"),
		UserID:           q.Get("user_id"),
		Vendor:           q.Get("vendor"),
		Model:            q.Get("model"),
		ActionID:         q.Get("action_id"),
		Status:           status,
		GroupByProjectID: q.Get("group_by_project_id") == "true",
		GroupByUserID:    q.Get("group_by_user_id") == "true",
		GroupByVendor:    q.Get("group_by_vendor") == "true",
		GroupByModel:     q.Get("group_by_model") == "true",
		GroupByActionID:  q.Get("group_by_action_id") == "true",
		GroupByStatus:    q.Get("group_by_status") == "true",
	}

	rows, truncated, err := s.service.GetSummary(r.Context(), params)
	if err != nil {
		log.Error().Err(err).Msg("failed to get usage summary")
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to get summary", "")
		return
	}

	respRows := make([]summaryRow, 0, len(rows))
	for _, row := range rows {
		// Like the other grouping fields, status is left out unless grouped by.
		var status string
		if params.GroupByStatus {
			status = row.Status.String()
		}
		respRows = append(respRows, summaryRow{
			Bucket:           row.Bucket.Format(time.RFC3339),
			ProjectID:        row.ProjectID,
			UserID:           row.UserID,
			Vendor:           row.Vendor,
			Model:            row.Model,
			ActionID:         row.ActionID,
			Status:           status,
			TokensInput:      row.TokensInput,
			TokensOutput:     row.TokensOutput,
			TokensTotal:      row.TokensTotal,
			RequestCount:     row.RequestCount,
			SumDurationMs:    row.SumDurationMs,
			CostNanoUSD:      row.CostNanoUSD,
			CacheReadTokens:  row.CacheReadTokens,
			CacheWriteTokens: row.CacheWriteTokens,
		})
	}

	writeJSON(w, http.StatusOK, getSummaryResponse{
		Rows:      respRows,
		Truncated: truncated,
	})
}

func httpRequestToEvent(req *recordEventRequest) (*domain.UsageEvent, error) {
	event := &domain.UsageEvent{
		UserID:           req.UserID,
		ProjectID:        req.ProjectID,
		ActionID:         req.ActionID,
		Vendor:           req.Vendor,
		Model:            req.Model,
		Status:           domain.StatusFromString(req.Status),
		DurationMs:       req.DurationMs,
		TokensInput:      req.TokensInput,
		TokensOutput:     req.TokensOutput,
		TokensTotal:      req.TokensTotal,
		ErrorClass:       req.ErrorClass,
		VendorHTTPStatus: req.VendorHTTPStatus,
		Meta:             req.Meta,
		OccurredAt:       req.OccurredAt,
		CostNanoUSD:      req.CostNanoUSD,
		CacheReadTokens:  req.CacheReadTokens,
		CacheWriteTokens: req.CacheWriteTokens,
	}
	if tc := req.ToolCall; tc != nil {
		event.ToolServer, event.ToolName, event.ResponseBytes = tc.Server, tc.Tool, tc.ResponseBytes
	}

	if err := event.EventID.UnmarshalText([]byte(req.EventID)); err != nil {
		return nil, fmt.Errorf("invalid event_id: %w", err)
	}

	if err := event.Validate(); err != nil {
		return nil, err
	}

	return event, nil
}

func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

// decodeBody reads a JSON body of at most domain.MaxRequestBytes, answering
// the client itself when it cannot.
func decodeBody(w http.ResponseWriter, r *http.Request, into any) bool {
	err := json.NewDecoder(http.MaxBytesReader(w, r.Body, domain.MaxRequestBytes)).Decode(into)
	var tooLarge *http.MaxBytesError
	switch {
	case err == nil:
		return true
	case errors.As(err, &tooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "request_too_large",
			fmt.Sprintf("request body exceeds %d bytes", domain.MaxRequestBytes), "")
	default:
		writeError(w, http.StatusBadRequest, "invalid_json", "Failed to parse request body", err.Error())
	}
	return false
}

func writeError(w http.ResponseWriter, status int, code, message, details string) {
	resp := errorResponse{
		Error:   message,
		Code:    code,
		Details: details,
	}
	writeJSON(w, status, resp)
}

// statusRecorder keeps the status code for the request metrics.
type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.code = code
	r.ResponseWriter.WriteHeader(code)
}

// instrument counts and times a route under its pattern, a bounded label set.
func instrument(pattern string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(rec, r)
		metrics.Requests.WithLabelValues("http", pattern, strconv.Itoa(rec.code)).Inc()
		metrics.RequestDuration.WithLabelValues("http", pattern).Observe(time.Since(start).Seconds())
	})
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Debug().
			Str("method", r.Method).
			Str("path", r.URL.Path).
			Dur("duration", time.Since(start)).
			Msg("HTTP request")
	})
}
