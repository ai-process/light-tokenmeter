package database

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestDescribeLeavesOutThePassword(t *testing.T) {
	for _, dsn := range []string{
		"postgres://tm:s3cret-pw@db.example:5433/usage?sslmode=disable",
		"host=db.example port=5433 user=tm password=s3cret-pw dbname=usage sslmode=disable",
	} {
		c, err := pgxpool.ParseConfig(dsn)
		if err != nil {
			t.Fatal(err)
		}
		got := describe(c)
		if strings.Contains(got, "s3cret-pw") {
			t.Fatalf("describe(%q) = %q leaks the password", dsn, got)
		}
		if got != "tm@db.example:5433/usage" {
			t.Errorf("describe(%q) = %q", dsn, got)
		}
	}
}
