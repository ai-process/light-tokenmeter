package service

import (
	"context"
	"sync/atomic"

	"github.com/rs/zerolog/log"

	"github.com/ai-process/light-tokenmeter/internal/domain"
	"github.com/ai-process/light-tokenmeter/internal/repository"
)

// AnalyticsReader defines the analytical query capabilities.
type AnalyticsReader interface {
	QuerySummary(ctx context.Context, params domain.SummaryParams) ([]domain.SummaryRow, bool, error)
	QueryUsageByMeta(ctx context.Context, params domain.MetaUsageParams) ([]domain.MetaUsageRow, bool, error)
	Ping(ctx context.Context) error
}

// UsageService handles business logic for usage events
type UsageService struct {
	repo *repository.Repository
	// Set once ClickHouse is reachable, which may be after startup.
	analytics       atomic.Pointer[AnalyticsReader]
	analyticsWanted atomic.Bool
}

// ExpectAnalytics marks ClickHouse as configured, so readiness can report
// it missing until WithAnalytics is called.
func (s *UsageService) ExpectAnalytics() { s.analyticsWanted.Store(true) }

// AnalyticsMissing reports ClickHouse configured but not connected yet.
func (s *UsageService) AnalyticsMissing() bool {
	return s.analyticsWanted.Load() && s.analytics.Load() == nil
}

// NewUsageService creates a new usage service
func NewUsageService(repo *repository.Repository) *UsageService {
	return &UsageService{repo: repo}
}

// WithAnalytics configures an analytical engine (e.g., ClickHouse).
func (s *UsageService) WithAnalytics(r AnalyticsReader) *UsageService {
	s.analytics.Store(&r)
	return s
}

// Analytics returns the configured AnalyticsReader, if any.
func (s *UsageService) Analytics() AnalyticsReader {
	if r := s.analytics.Load(); r != nil {
		return *r
	}
	return nil
}

// RecordEvent records a single usage event
func (s *UsageService) RecordEvent(ctx context.Context, event *domain.UsageEvent) (bool, error) {
	created, err := s.repo.InsertOne(ctx, event)
	if err != nil {
		return false, err
	}

	if created {
		log.Debug().
			Str("event_id", event.EventID.String()).
			Str("project_id", event.ProjectID).
			Str("vendor", event.Vendor).
			Str("model", event.Model).
			Int64("tokens_total", event.TokensTotal).
			Msg("recorded usage event")
	} else {
		log.Debug().
			Str("event_id", event.EventID.String()).
			Msg("duplicate event (idempotent)")
	}

	return created, nil
}

// RecordEvents records multiple usage events in a batch
func (s *UsageService) RecordEvents(ctx context.Context, events []*domain.UsageEvent) (created, duplicates int, err error) {
	inserted, err := s.repo.InsertBatch(ctx, events)
	if err != nil {
		return 0, 0, err
	}
	created, duplicates = len(inserted), len(events)-len(inserted)

	log.Debug().
		Int("created", created).
		Int("duplicates", duplicates).
		Int("total", len(events)).
		Msg("recorded usage events batch")

	return created, duplicates, nil
}

// GetSummary returns aggregated usage statistics
func (s *UsageService) GetSummary(ctx context.Context, params domain.SummaryParams) ([]domain.SummaryRow, bool, error) {
	if a := s.Analytics(); a != nil {
		rows, truncated, err := a.QuerySummary(ctx, params)
		if err == nil || ctx.Err() != nil {
			return rows, truncated, err
		}
		log.Warn().Err(err).Msg("clickhouse summary failed; answering from postgres")
	}

	rows, truncated, err := s.repo.QuerySummary(ctx, params)
	if err != nil {
		return nil, false, err
	}

	log.Debug().
		Int("rows", len(rows)).
		Bool("truncated", truncated).
		Time("from", params.From).
		Time("to", params.To).
		Msg("queried usage summary")

	return rows, truncated, nil
}

// GetUsageByMeta returns usage aggregated over events carrying a meta
// key/value, split by an optional second meta key plus vendor and model.
func (s *UsageService) GetUsageByMeta(ctx context.Context, params domain.MetaUsageParams) ([]domain.MetaUsageRow, bool, error) {
	if a := s.Analytics(); a != nil {
		rows, truncated, err := a.QueryUsageByMeta(ctx, params)
		if err == nil || ctx.Err() != nil {
			return rows, truncated, err
		}
		log.Warn().Err(err).Msg("clickhouse usage-by-meta failed; answering from postgres")
	}

	rows, truncated, err := s.repo.QueryUsageByMeta(ctx, params)
	if err != nil {
		return nil, false, err
	}

	log.Debug().
		Int("rows", len(rows)).
		Bool("truncated", truncated).
		Str("meta_key", params.MetaKey).
		Str("group_by", params.GroupByMetaKey).
		Msg("queried usage by meta")

	return rows, truncated, nil
}
