package config

import (
	"os"
	"strconv"
	"time"
)

// Config holds the application configuration
type Config struct {
	DatabaseURL     string
	GRPCAddr        string
	HTTPAddr        string
	LogLevel        string
	LogFormat       string
	ShutdownTimeout time.Duration
	DBMaxConns      int32
	DBMinConns      int32

	// Auth: entries name:scope[+scope]:sha256hex, inline or in a file.
	Tokens     string
	TokensFile string

	// ClickHouse
	ClickHouseEnabled       bool
	ClickHouseAddr          string
	ClickHouseDatabase      string
	ClickHouseUser          string
	ClickHousePassword      string
	ClickHouseBatchSize     int
	ClickHouseFlushInterval time.Duration

	// Sentry
	SentryDSN             string
	SentryEnvironment     string
	SentryRelease         string
	SentryTracing         bool
	SentryDebug           bool
	SentryWithBreadcrumbs bool
}

// Load loads configuration from environment variables
func Load() *Config {
	return &Config{
		DatabaseURL:             getEnv("DATABASE_URL", "postgres://tokenmeter:tokenmeter@localhost:5432/tokenmeter?sslmode=disable"),
		GRPCAddr:                getEnv("GRPC_ADDR", ":9090"),
		HTTPAddr:                getEnv("HTTP_ADDR", ":8080"),
		LogLevel:                getEnv("LOG_LEVEL", "info"),
		LogFormat:               getEnv("LOG_FORMAT", "json"),
		ShutdownTimeout:         getDurationEnv("SHUTDOWN_TIMEOUT", 30*time.Second),
		DBMaxConns:              int32(getIntEnv("DB_MAX_CONNS", 25)),
		DBMinConns:              int32(getIntEnv("DB_MIN_CONNS", 5)),
		Tokens:                  getEnv("TOKENMETER_TOKENS", ""),
		TokensFile:              getEnv("TOKENMETER_TOKENS_FILE", ""),
		ClickHouseEnabled:       getBoolEnv("CLICKHOUSE_ENABLED", false),
		ClickHouseAddr:          getEnv("CLICKHOUSE_ADDR", "localhost:9000"),
		ClickHouseDatabase:      getEnv("CLICKHOUSE_DATABASE", "tokenmeter"),
		ClickHouseUser:          getEnv("CLICKHOUSE_USER", "default"),
		ClickHousePassword:      getEnv("CLICKHOUSE_PASSWORD", ""),
		ClickHouseBatchSize:     getIntEnv("CLICKHOUSE_BATCH_SIZE", 5000),
		ClickHouseFlushInterval: getDurationEnv("CLICKHOUSE_FLUSH_INTERVAL", 1*time.Second),
		SentryDSN:               getEnv("SENTRY_DSN", ""),
		SentryEnvironment:       getEnv("SENTRY_ENVIRONMENT", "development"),
		SentryRelease:           getEnv("SENTRY_RELEASE", ""),
		SentryTracing:           getEnv("SENTRY_ENABLE_TRACING", "false") == "true",
		SentryDebug:             getEnv("SENTRY_DEBUG", "false") == "true",
		SentryWithBreadcrumbs:   getEnv("SENTRY_WITH_BREADCRUMBS", "false") == "true",
	}
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getIntEnv(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		if i, err := strconv.Atoi(value); err == nil {
			return i
		}
	}
	return defaultValue
}

func getDurationEnv(key string, defaultValue time.Duration) time.Duration {
	if value := os.Getenv(key); value != "" {
		if d, err := time.ParseDuration(value); err == nil {
			return d
		}
	}
	return defaultValue
}

func getBoolEnv(key string, defaultValue bool) bool {
	if value := os.Getenv(key); value != "" {
		return value == "true" || value == "1"
	}
	return defaultValue
}
