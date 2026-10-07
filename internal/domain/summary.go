package domain

import "time"

// SummaryParams holds the parameters for querying usage summary
type SummaryParams struct {
	From time.Time
	To   time.Time

	// Filters
	ProjectID string
	UserID    string
	Vendor    string
	Model     string
	ActionID  string
	Status    Status

	// Group by dimensions (day is always included)
	GroupByProjectID bool
	GroupByUserID    bool
	GroupByVendor    bool
	GroupByModel     bool
	GroupByActionID  bool
	GroupByStatus    bool
}

// SummaryRow represents a single row in the usage summary
type SummaryRow struct {
	Bucket time.Time

	// Dimension values (only populated if grouped by)
	ProjectID string
	UserID    string
	Vendor    string
	Model     string
	ActionID  string
	Status    Status

	// Aggregated values
	TokensInput   int64
	TokensOutput  int64
	TokensTotal   int64
	RequestCount  int64
	SumDurationMs int64
}
