package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/reflection/grpc_reflection_v1"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/ai-process/light-tokenmeter/internal/auth"
	"github.com/ai-process/light-tokenmeter/internal/domain"
	"github.com/ai-process/light-tokenmeter/internal/metrics"

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

func testVerifier(t *testing.T) *auth.Verifier {
	t.Helper()
	sum := func(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
	v, err := auth.Parse("ktms:write:" + sum("w-token") + ",console:read:" + sum("r-token"))
	require.NoError(t, err)
	return v
}

func TestHTTPServer_Auth(t *testing.T) {
	h := NewHTTPServerWithService(nil, nil, ":0", testVerifier(t)).server.Handler
	do := func(method, path, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewReader([]byte("{")))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}

	assert.Equal(t, http.StatusOK, do(http.MethodGet, "/healthz", "").Code)
	for _, path := range []string{"/v1/usage/events", "/v1/usage/events:batch"} {
		w := do(http.MethodPost, path, "")
		assert.Equal(t, http.StatusUnauthorized, w.Code, path)
		assert.Contains(t, w.Body.String(), `"code":"unauthenticated"`, path)
		assert.Equal(t, http.StatusForbidden, do(http.MethodPost, path, "r-token").Code, path)
		// The malformed body proves the request got past auth.
		assert.Equal(t, http.StatusBadRequest, do(http.MethodPost, path, "w-token").Code, path)
	}
	assert.Equal(t, http.StatusUnauthorized, do(http.MethodGet, "/v1/usage/summary", "").Code)
	assert.Equal(t, http.StatusForbidden, do(http.MethodGet, "/v1/usage/summary", "w-token").Code)
	assert.Equal(t, http.StatusBadRequest, do(http.MethodGet, "/v1/usage/summary", "r-token").Code)
}

func TestGRPCServer_Auth(t *testing.T) {
	lis := bufconn.Listen(1 << 20)
	srv := NewGRPCServerWithService(nil, "", testVerifier(t))
	go func() { _ = srv.server.Serve(lis) }()
	t.Cleanup(srv.server.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	client := pb.NewUsageServiceClient(conn)

	withToken := func(token string) context.Context {
		return metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+token)
	}
	code := func(err error) codes.Code { return status.Code(err) }

	// Empty requests fail validation, which only runs once auth passes.
	_, err = client.RecordUsageEvents(context.Background(), &pb.RecordUsageEventsRequest{})
	assert.Equal(t, codes.Unauthenticated, code(err))
	_, err = client.RecordUsageEvents(withToken("r-token"), &pb.RecordUsageEventsRequest{})
	assert.Equal(t, codes.PermissionDenied, code(err))
	_, err = client.RecordUsageEvents(withToken("w-token"), &pb.RecordUsageEventsRequest{})
	assert.Equal(t, codes.InvalidArgument, code(err))
	_, err = client.RecordUsageEvent(withToken("w-token"), &pb.RecordUsageEventRequest{})
	assert.Equal(t, codes.InvalidArgument, code(err))

	_, err = client.GetUsageSummary(withToken("w-token"), &pb.GetUsageSummaryRequest{})
	assert.Equal(t, codes.PermissionDenied, code(err))
	_, err = client.GetUsageSummary(withToken("r-token"), &pb.GetUsageSummaryRequest{})
	assert.Equal(t, codes.InvalidArgument, code(err))
	_, err = client.GetUsageByMeta(withToken("r-token"), &pb.GetUsageByMetaRequest{})
	assert.Equal(t, codes.InvalidArgument, code(err))

	hc, err := healthpb.NewHealthClient(conn).Check(context.Background(), &healthpb.HealthCheckRequest{})
	require.NoError(t, err, "health must not need a token")
	assert.Equal(t, healthpb.HealthCheckResponse_SERVING, hc.Status)

	listServices := func(ctx context.Context) error {
		stream, err := grpc_reflection_v1.NewServerReflectionClient(conn).ServerReflectionInfo(ctx)
		require.NoError(t, err)
		// A refused stream may fail Send with io.EOF; Recv carries the real status.
		_ = stream.Send(&grpc_reflection_v1.ServerReflectionRequest{
			MessageRequest: &grpc_reflection_v1.ServerReflectionRequest_ListServices{},
		})
		_, err = stream.Recv()
		return err
	}
	assert.Equal(t, codes.Unauthenticated, code(listServices(context.Background())), "reflection without a token")
	assert.NoError(t, listServices(withToken("r-token")), "reflection with any token")
}

func TestGRPCScopesCoverEveryRPC(t *testing.T) {
	for _, m := range pb.UsageService_ServiceDesc.Methods {
		full := "/" + pb.UsageService_ServiceDesc.ServiceName + "/" + m.MethodName
		_, ok := grpcScopes[full]
		assert.True(t, ok, "%s has no scope", full)
	}
}

// KTMS drops the event this message names and resends the rest; the prefix
// is part of the 1.x API (README "API stability").
func TestRejectedBatchNamesTheBadEvent(t *testing.T) {
	srv := NewGRPCServerWithService(nil, "", nil)
	good := &pb.UsageEvent{EventId: uuid.NewString(), UserId: "u", ProjectId: "p", ActionId: "a",
		Vendor: "openai", Model: "m", Status: pb.Status_STATUS_SUCCESS}
	bad := &pb.UsageEvent{EventId: "not-a-uuid"}
	_, err := srv.RecordUsageEvents(context.Background(), &pb.RecordUsageEventsRequest{Events: []*pb.UsageEvent{good, bad}})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	assert.True(t, strings.HasPrefix(status.Convert(err).Message(), "invalid event at index 1: "), status.Convert(err).Message())
}

func TestHTTPServer_BodyLimit(t *testing.T) {
	h := NewHTTPServer(nil, ":0").server.Handler
	big := bytes.Repeat([]byte(" "), domain.MaxRequestBytes+1)
	req := httptest.NewRequest(http.MethodPost, "/v1/usage/events:batch", bytes.NewReader(big))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
	assert.Contains(t, w.Body.String(), `"code":"request_too_large"`)
}

func TestRequestAndEventMetrics(t *testing.T) {
	h := NewHTTPServer(nil, ":0").server.Handler
	probeSeries := func() int {
		n := 0
		mfs, err := metrics.Registry.Gather()
		require.NoError(t, err)
		for _, mf := range mfs {
			for _, m := range mf.GetMetric() {
				for _, l := range m.GetLabel() {
					if l.GetName() == "route" && (strings.Contains(l.GetValue(), "healthz") || strings.Contains(l.GetValue(), "readyz") || strings.Contains(l.GetValue(), "grpc.health")) {
						n++
					}
				}
			}
		}
		return n
	}
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz", nil))

	invalid := metrics.Events.WithLabelValues("invalid")
	before := testutil.ToFloat64(invalid)
	body, _ := json.Marshal(recordEventsRequest{Events: []recordEventRequest{{EventID: "x"}, {EventID: "y"}, {EventID: "z"}}})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/usage/events:batch", bytes.NewReader(body)))
	require.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, before+3, testutil.ToFloat64(invalid), "a refused batch counts all its events")
	assert.GreaterOrEqual(t, testutil.ToFloat64(metrics.Requests.WithLabelValues("http", "POST /v1/usage/events:batch", "400")), float64(1), "the refusal is counted under its route")

	srv := NewGRPCServerWithService(nil, "", testVerifier(t))
	lis := bufconn.Listen(1 << 20)
	go func() { _ = srv.server.Serve(lis) }()
	t.Cleanup(srv.server.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	_, err = healthpb.NewHealthClient(conn).Check(context.Background(), &healthpb.HealthCheckRequest{})
	require.NoError(t, err)
	assert.Zero(t, probeSeries(), "probes are not API traffic")
	refused := metrics.Requests.WithLabelValues("grpc", pb.UsageService_RecordUsageEvents_FullMethodName, "Unauthenticated")
	before = testutil.ToFloat64(refused)
	_, err = pb.NewUsageServiceClient(conn).RecordUsageEvents(context.Background(), &pb.RecordUsageEventsRequest{})
	require.Equal(t, codes.Unauthenticated, status.Code(err))
	assert.Equal(t, before+1, testutil.ToFloat64(refused), "calls refused by auth are counted")
}
