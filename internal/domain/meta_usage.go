package domain

// MetaUsageParams holds the parameters for aggregating usage over events
// carrying a given meta key/value (e.g. one batch job's events).
type MetaUsageParams struct {
	// MetaKey/MetaValue select the events (required).
	MetaKey   string
	MetaValue string

	// GroupByMetaKey optionally splits rows by a second meta key
	// (e.g. "gentask_id"); empty groups everything under GroupValue "".
	GroupByMetaKey string

	// ProjectID optionally narrows to one project.
	ProjectID string
}

// MetaUsageRow is one aggregate bucket: (GroupValue, Vendor, Model).
type MetaUsageRow struct {
	GroupValue   string
	Vendor       string
	Model        string
	TokensInput  int64
	TokensOutput int64
	TokensTotal  int64
	RequestCount int64
	CostNanoUSD  int64
}
