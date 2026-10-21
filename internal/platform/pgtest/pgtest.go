// Package pgtest gives integration tests their own freshly migrated
// PostgreSQL schema, so test packages running in parallel never share
// tables. Tests skip when no database is reachable.
package pgtest

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jpamaze/amazeA2pSMS/internal/platform/pg"
	"github.com/jpamaze/amazeA2pSMS/migrations"
)

var unsafe = regexp.MustCompile(`[^a-z0-9_]`)

// Pool returns a pool whose search_path is a new schema named after the
// test, with all migrations applied. POSTGRES_TEST_URL picks the database
// (default: local sms_test). The schema is dropped when the test ends.
func Pool(t testing.TB) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("POSTGRES_TEST_URL")
	if url == "" {
		url = "postgres://sms:sms@localhost:5432/sms_test?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	admin, err := pgxpool.New(ctx, url)
	if err == nil {
		err = admin.Ping(ctx)
	}
	if err != nil {
		t.Skipf("postgres not reachable: %v", err)
	}
	schema := "t_" + unsafe.ReplaceAllString(strings.ToLower(t.Name()), "_")
	if len(schema) > 60 {
		schema = schema[:60]
	}
	if _, err := admin.Exec(ctx, fmt.Sprintf(`CREATE EXTENSION IF NOT EXISTS pgcrypto SCHEMA public;
		DROP SCHEMA IF EXISTS %[1]s CASCADE; CREATE SCHEMA %[1]s`, schema)); err != nil {
		t.Fatal(err)
	}

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), fmt.Sprintf(`DROP SCHEMA IF EXISTS %s CASCADE`, schema))
		admin.Close()
	})
	if _, err := pg.Migrate(ctx, pool, migrations.Postgres, "postgres", slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	return pool
}
