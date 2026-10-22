package clickhouse

// Config holds the ClickHouse connection settings.
type Config struct {
	Addr     string
	Database string
	User     string
	Password string
}
