package pg

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"
)

// migrateLockID serialises concurrent migrators (any constant works).
const migrateLockID = 727_000_001

// Migrate applies every *.sql file in dir of fsys that is not yet recorded in
// schema_migrations, in file-name order, each in its own transaction.
// It returns the versions applied.
func Migrate(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS, dir string, log *slog.Logger) ([]string, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("migrate: acquire: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrateLockID); err != nil {
		return nil, fmt.Errorf("migrate: lock: %w", err)
	}
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, migrateLockID) //nolint:errcheck

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    text PRIMARY KEY,
		applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return nil, fmt.Errorf("migrate: create table: %w", err)
	}

	names, err := fs.Glob(fsys, dir+"/*.sql")
	if err != nil {
		return nil, err
	}
	sort.Strings(names)

	var applied []string
	for _, name := range names {
		version := name[len(dir)+1:]
		var exists bool
		if err := conn.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, version).Scan(&exists); err != nil {
			return applied, err
		}
		if exists {
			continue
		}
		sql, err := fs.ReadFile(fsys, name)
		if err != nil {
			return applied, err
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return applied, err
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			_ = tx.Rollback(ctx)
			return applied, fmt.Errorf("migrate: %s: %w", version, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, version); err != nil {
			_ = tx.Rollback(ctx)
			return applied, err
		}
		if err := tx.Commit(ctx); err != nil {
			return applied, err
		}
		log.Info("migration applied", "version", version)
		applied = append(applied, version)
	}
	return applied, nil
}
