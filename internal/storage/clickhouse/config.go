package clickhouse

import "time"

// Config holds the ClickHouse connection and batching settings.
type Config struct {
	Addr          string
	Database      string
	User          string
	Password      string
	BatchSize     int
	FlushInterval time.Duration
}
