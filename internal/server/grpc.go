package server

import (
	"context"
	"fmt"
	"net"

	"github.com/rs/zerolog/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/ai-process/light-tokenmeter/internal/auth"
	"github.com/ai-process/light-tokenmeter/internal/domain"
	"github.com/ai-process/light-tokenmeter/internal/repository"
	"github.com/ai-process/light-tokenmeter/internal/service"

	pb "github.com/ai-process/light-tokenmeter/gen/usage/v1"
)

// GRPCServer wraps the gRPC server implementation
type GRPCServer struct {
	pb.UnimplementedUsageServiceServer
	service *service.UsageService
	server  *grpc.Server
	addr    string
}

// NewGRPCServer creates a new gRPC server with a repository.
func NewGRPCServer(repo *repository.Repository, addr string) *GRPCServer {
	return NewGRPCServerWithService(service.NewUsageService(repo), addr, nil)
}

// grpcScopes lists every UsageService RPC; one missing here is refused.
var grpcScopes = map[string]auth.Scope{
	pb.UsageService_RecordUsageEvent_FullMethodName:  auth.Write,
	pb.UsageService_RecordUsageEvents_FullMethodName: auth.Write,
	pb.UsageService_GetUsageSummary_FullMethodName:   auth.Read,
	pb.UsageService_GetUsageByMeta_FullMethodName:    auth.Read,
}

// NewGRPCServerWithService creates a new gRPC server with an existing usage
// service. A nil or empty verifier serves without authentication.
func NewGRPCServerWithService(usageService *service.UsageService, addr string, verifier *auth.Verifier) *GRPCServer {
	srv := grpc.NewServer(
		// Auth runs first, so refusals are logged at Warn rather than as errors.
		grpc.ChainUnaryInterceptor(auth.UnaryInterceptor(verifier, grpcScopes), loggingInterceptor),
		grpc.StreamInterceptor(auth.StreamInterceptor(verifier)),
	)

	grpcServer := &GRPCServer{
		service: usageService,
		server:  srv,
		addr:    addr,
	}

	pb.RegisterUsageServiceServer(srv, grpcServer)
	healthpb.RegisterHealthServer(srv, health.NewServer())
	reflection.Register(srv)

	return grpcServer
}

// Start starts the gRPC server
func (s *GRPCServer) Start() error {
	lis, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", s.addr, err)
	}

	log.Info().Str("addr", s.addr).Msg("starting gRPC server")
	return s.server.Serve(lis)
}

// Stop gracefully stops the gRPC server
func (s *GRPCServer) Stop() {
	log.Info().Msg("stopping gRPC server")
	s.server.GracefulStop()
}

// RecordUsageEvent records a single usage event
func (s *GRPCServer) RecordUsageEvent(ctx context.Context, req *pb.RecordUsageEventRequest) (*pb.RecordUsageEventResponse, error) {
	if req.Event == nil {
		return nil, status.Error(codes.InvalidArgument, "event is required")
	}

	event, err := protoToEvent(req.Event)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid event: %v", err)
	}

	created, err := s.service.RecordEvent(ctx, event)
	if err != nil {
		log.Error().Err(err).Str("event_id", event.EventID.String()).Msg("failed to record event")
		return nil, status.Errorf(codes.Internal, "failed to record event: %v", err)
	}

	return &pb.RecordUsageEventResponse{Created: created}, nil
}

// RecordUsageEvents records multiple usage events in a batch
func (s *GRPCServer) RecordUsageEvents(ctx context.Context, req *pb.RecordUsageEventsRequest) (*pb.RecordUsageEventsResponse, error) {
	if len(req.Events) == 0 {
		return nil, status.Error(codes.InvalidArgument, "events list is empty")
	}

	if len(req.Events) > domain.MaxBatchSize {
		return nil, status.Errorf(codes.InvalidArgument, "batch size exceeds maximum of %d events", domain.MaxBatchSize)
	}

	events := make([]*domain.UsageEvent, 0, len(req.Events))
	for i, pbEvent := range req.Events {
		event, err := protoToEvent(pbEvent)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid event at index %d: %v", i, err)
		}
		events = append(events, event)
	}

	created, duplicates, err := s.service.RecordEvents(ctx, events)
	if err != nil {
		log.Error().Err(err).Int("batch_size", len(events)).Msg("failed to record events batch")
		return nil, status.Errorf(codes.Internal, "failed to record events: %v", err)
	}

	return &pb.RecordUsageEventsResponse{
		CreatedCount:   int32(created),
		DuplicateCount: int32(duplicates),
	}, nil
}

// GetUsageSummary returns aggregated usage statistics
func (s *GRPCServer) GetUsageSummary(ctx context.Context, req *pb.GetUsageSummaryRequest) (*pb.GetUsageSummaryResponse, error) {
	if req.From == nil || req.To == nil {
		return nil, status.Error(codes.InvalidArgument, "from and to timestamps are required")
	}

	params := domain.SummaryParams{
		From:             req.From.AsTime(),
		To:               req.To.AsTime(),
		ProjectID:        req.ProjectId,
		UserID:           req.UserId,
		Vendor:           req.Vendor,
		Model:            req.Model,
		ActionID:         req.ActionId,
		Status:           protoStatusToDomain(req.Status),
		GroupByProjectID: req.GroupByProjectId,
		GroupByUserID:    req.GroupByUserId,
		GroupByVendor:    req.GroupByVendor,
		GroupByModel:     req.GroupByModel,
		GroupByActionID:  req.GroupByActionId,
		GroupByStatus:    req.GroupByStatus,
	}

	rows, truncated, err := s.service.GetSummary(ctx, params)
	if err != nil {
		log.Error().Err(err).Msg("failed to get usage summary")
		return nil, status.Errorf(codes.Internal, "failed to get summary: %v", err)
	}

	pbRows := make([]*pb.UsageSummaryRow, 0, len(rows))
	for _, row := range rows {
		pbRows = append(pbRows, &pb.UsageSummaryRow{
			Bucket:        timestamppb.New(row.Bucket),
			ProjectId:     row.ProjectID,
			UserId:        row.UserID,
			Vendor:        row.Vendor,
			Model:         row.Model,
			ActionId:      row.ActionID,
			Status:        domainStatusToProto(row.Status),
			TokensInput:   row.TokensInput,
			TokensOutput:  row.TokensOutput,
			TokensTotal:   row.TokensTotal,
			RequestCount:  row.RequestCount,
			SumDurationMs: row.SumDurationMs,
			CostNanousd:   row.CostNanoUSD,
		})
	}

	return &pb.GetUsageSummaryResponse{
		Rows:      pbRows,
		Truncated: truncated,
	}, nil
}

// GetUsageByMeta returns usage aggregated over events carrying a meta
// key/value (e.g. one batch job), split by an optional second meta key
// plus vendor and model.
func (s *GRPCServer) GetUsageByMeta(ctx context.Context, req *pb.GetUsageByMetaRequest) (*pb.GetUsageByMetaResponse, error) {
	if req.MetaKey == "" || req.MetaValue == "" {
		return nil, status.Error(codes.InvalidArgument, "meta_key and meta_value are required")
	}

	rows, truncated, err := s.service.GetUsageByMeta(ctx, domain.MetaUsageParams{
		MetaKey:        req.MetaKey,
		MetaValue:      req.MetaValue,
		GroupByMetaKey: req.GroupByMetaKey,
		ProjectID:      req.ProjectId,
	})
	if err != nil {
		log.Error().Err(err).Str("meta_key", req.MetaKey).Msg("failed to get usage by meta")
		return nil, status.Errorf(codes.Internal, "failed to get usage by meta: %v", err)
	}

	pbRows := make([]*pb.UsageByMetaRow, 0, len(rows))
	for _, row := range rows {
		pbRows = append(pbRows, &pb.UsageByMetaRow{
			GroupValue:   row.GroupValue,
			Vendor:       row.Vendor,
			Model:        row.Model,
			TokensInput:  row.TokensInput,
			TokensOutput: row.TokensOutput,
			TokensTotal:  row.TokensTotal,
			RequestCount: row.RequestCount,
			CostNanousd:  row.CostNanoUSD,
		})
	}
	return &pb.GetUsageByMetaResponse{
		Rows:      pbRows,
		Truncated: truncated,
	}, nil
}

// protoToEvent converts a protobuf UsageEvent to domain.UsageEvent
func protoToEvent(pbEvent *pb.UsageEvent) (*domain.UsageEvent, error) {
	event := &domain.UsageEvent{
		UserID:           pbEvent.UserId,
		ProjectID:        pbEvent.ProjectId,
		ActionID:         pbEvent.ActionId,
		Vendor:           pbEvent.Vendor,
		Model:            pbEvent.Model,
		Status:           protoStatusToDomain(pbEvent.Status),
		DurationMs:       pbEvent.DurationMs,
		TokensInput:      pbEvent.TokensInput,
		TokensOutput:     pbEvent.TokensOutput,
		TokensTotal:      pbEvent.TokensTotal,
		ErrorClass:       pbEvent.ErrorClass,
		VendorHTTPStatus: pbEvent.VendorHttpStatus,
		Meta:             pbEvent.Meta,
		CostNanoUSD:      pbEvent.CostNanousd,
	}

	// Parse event_id
	if err := event.EventID.UnmarshalText([]byte(pbEvent.EventId)); err != nil {
		return nil, fmt.Errorf("invalid event_id: %w", err)
	}

	// Set occurred_at if provided
	if pbEvent.OccurredAt != nil {
		t := pbEvent.OccurredAt.AsTime()
		event.OccurredAt = &t
	}

	// Validate the event
	if err := event.Validate(); err != nil {
		return nil, err
	}

	return event, nil
}

func protoStatusToDomain(s pb.Status) domain.Status {
	switch s {
	case pb.Status_STATUS_SUCCESS:
		return domain.StatusSuccess
	case pb.Status_STATUS_ERROR:
		return domain.StatusError
	case pb.Status_STATUS_CANCELLED:
		return domain.StatusCancelled
	case pb.Status_STATUS_TIMEOUT:
		return domain.StatusTimeout
	default:
		return domain.StatusUnspecified
	}
}

func domainStatusToProto(s domain.Status) pb.Status {
	switch s {
	case domain.StatusSuccess:
		return pb.Status_STATUS_SUCCESS
	case domain.StatusError:
		return pb.Status_STATUS_ERROR
	case domain.StatusCancelled:
		return pb.Status_STATUS_CANCELLED
	case domain.StatusTimeout:
		return pb.Status_STATUS_TIMEOUT
	default:
		return pb.Status_STATUS_UNSPECIFIED
	}
}

// loggingInterceptor logs gRPC requests
func loggingInterceptor(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
	log.Debug().Str("method", info.FullMethod).Msg("gRPC request")
	resp, err := handler(ctx, req)
	if err != nil {
		log.Error().Err(err).Str("method", info.FullMethod).Msg("gRPC error")
	}
	return resp, err
}
