// Package testdb gives each test a Postgres database of its own, created,
// migrated and dropped around it, so no test touches data it did not make.
package testdb

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/ai-process/light-tokenmeter/internal/database"
)

const prefix = "tm_test_"

// staleAfter bounds how long a database left by an interrupted run lives.
const staleAfter = time.Hour

var sweepOnce sync.Once

// Unavailable skips the test, or fails it when REQUIRE_DB is set (CI).
func Unavailable(t testing.TB, format string, args ...any) {
	t.Helper()
	if os.Getenv("REQUIRE_DB") != "" {
		t.Fatalf(format, args...)
	}
	t.Skipf(format, args...)
}

// Name builds a test database name stamped with the server's clock, so
// IsStale on another machine judges its age by the same clock.
func Name(prefix string, serverEpoch int64) string {
	return fmt.Sprintf("%s%d_%s", prefix, serverEpoch, uuid.NewString()[:8])
}

// IsStale reports a database made by Name more than staleAfter ago.
func IsStale(name, prefix string, serverEpoch int64) bool {
	ts, _, _ := strings.Cut(strings.TrimPrefix(name, prefix), "_")
	sec, err := strconv.ParseInt(ts, 10, 64)
	return err == nil && serverEpoch-sec > int64(staleAfter/time.Second)
}

// Postgres returns a pool on a fresh, migrated database. DATABASE_URL (URL
// or key=value form) names the server; the default is the local test one.
func Postgres(t testing.TB) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://tokenmeter:tokenmeter@localhost:5433/tokenmeter?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		Unavailable(t, "cannot reach test postgres: %v", err)
		return nil
	}
	t.Cleanup(admin.Close)
	var now int64
	if err := admin.QueryRow(ctx, "SELECT extract(epoch FROM now())::bigint").Scan(&now); err != nil {
		Unavailable(t, "cannot reach test postgres: %v", err)
		return nil
	}
	sweepOnce.Do(func() { sweep(t, admin, now) })

	name := Name(prefix, now)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "42501" {
			Unavailable(t, "cannot create a test database (the role needs CREATEDB): %v", err)
			return nil
		}
		t.Fatalf("create test database %s: %v", name, err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Logf("drop test database %s: %v", name, err)
		}
	})

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse DATABASE_URL: %v", err)
	}
	cfg.ConnConfig.Database = name
	// A registered config is a DSN database/sql understands, whatever form
	// DATABASE_URL took.
	registered := stdlib.RegisterConnConfig(cfg.ConnConfig.Copy())
	t.Cleanup(func() { stdlib.UnregisterConnConfig(registered) })
	if err := database.RunMigrations(ctx, registered); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// sweep drops test databases an interrupted run left behind. It has its own
// deadline, so a long cleanup never eats the test's setup time.
func sweep(t testing.TB, admin *pgxpool.Pool, now int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	rows, err := admin.Query(ctx, "SELECT datname FROM pg_database WHERE datname LIKE 'tm\\_test\\_%'")
	if err != nil {
		t.Logf("list stale test databases: %v", err)
		return
	}
	var stale []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err == nil && IsStale(name, prefix, now) {
			stale = append(stale, name)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Logf("list stale test databases: %v", err)
	}
	for _, name := range stale {
		if _, err := admin.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Logf("drop stale test database %s: %v", name, err)
		}
	}
}
