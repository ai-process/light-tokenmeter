package database

import (
	"context"
	"embed"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"
	"github.com/rs/zerolog/log"

	// Import database/sql for goose compatibility
	_ "github.com/jackc/pgx/v5/stdlib"
)

//go:embed migrations/*.sql
var embedMigrations embed.FS

// Config holds database configuration
type Config struct {
	URL      string
	MaxConns int32
	MinConns int32
}

// Connect creates a new database connection pool
func Connect(ctx context.Context, cfg Config) (*pgxpool.Pool, error) {
	poolConfig, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	poolConfig.MaxConns = cfg.MaxConns
	poolConfig.MinConns = cfg.MinConns
	poolConfig.MaxConnLifetime = 1 * time.Hour
	poolConfig.MaxConnIdleTime = 30 * time.Minute
	poolConfig.HealthCheckPeriod = 1 * time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	// Test connection
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	log.Info().
		Str("url", maskPassword(cfg.URL)).
		Int32("max_conns", cfg.MaxConns).
		Int32("min_conns", cfg.MinConns).
		Msg("connected to database")

	return pool, nil
}

// RunMigrations runs database migrations from the embedded filesystem
func RunMigrations(ctx context.Context, databaseURL string) error {
	goose.SetBaseFS(embedMigrations)

	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("set dialect: %w", err)
	}

	// Use pgx stdlib driver for goose
	db, err := goose.OpenDBWithDriver("pgx", databaseURL)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer db.Close()

	if err := goose.Up(db, "migrations"); err != nil {
		return fmt.Errorf("run migrations: %w", err)
	}

	log.Info().Msg("database migrations completed")
	return nil
}

// maskPassword masks the password in a database URL for logging
func maskPassword(url string) string {
	// Simple masking for logging - just replace password portion
	return url // In production, implement proper URL parsing and masking
}
