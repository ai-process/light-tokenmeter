package service

import (
	"context"

	"github.com/rs/zerolog/log"

	"github.com/ai-process/light-tokenmeter/internal/domain"
	"github.com/ai-process/light-tokenmeter/internal/repository"
)

// UsageService handles business logic for usage events
type UsageService struct {
	repo *repository.Repository
}

// NewUsageService creates a new usage service
func NewUsageService(repo *repository.Repository) *UsageService {
	return &UsageService{repo: repo}
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
	created, duplicates, err = s.repo.InsertBatch(ctx, events)
	if err != nil {
		return 0, 0, err
	}

	log.Debug().
		Int("created", created).
		Int("duplicates", duplicates).
		Int("total", len(events)).
		Msg("recorded usage events batch")

	return created, duplicates, nil
}

// GetSummary returns aggregated usage statistics
func (s *UsageService) GetSummary(ctx context.Context, params domain.SummaryParams) ([]domain.SummaryRow, bool, error) {
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
