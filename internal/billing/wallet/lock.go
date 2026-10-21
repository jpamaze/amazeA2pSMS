package wallet

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SyncLockID is the PostgreSQL advisory lock that orders balance changes
// against a resync. Every change made to a balance in both PostgreSQL and
// Redis (rater batches, top-ups, transfers, refunds) runs holding it shared;
// a resync holds it exclusively, so it never sees one side changed without
// the other. Credit limits are set, not added, and need no lock.
const SyncLockID int64 = 727_000_002

// Shared runs fn on one connection that holds the sync lock shared.
func Shared(ctx context.Context, pool *pgxpool.Pool, fn func(*pgxpool.Conn) error) error {
	return withLock(ctx, pool, "pg_advisory_lock_shared", "pg_advisory_unlock_shared", fn)
}

// Exclusive runs fn on one connection that holds the sync lock exclusively:
// no balance changes while it runs.
func Exclusive(ctx context.Context, pool *pgxpool.Pool, fn func(*pgxpool.Conn) error) error {
	return withLock(ctx, pool, "pg_advisory_lock", "pg_advisory_unlock", fn)
}

func withLock(ctx context.Context, pool *pgxpool.Pool, lock, unlock string, fn func(*pgxpool.Conn) error) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("wallet lock: %w", err)
	}
	if _, err := conn.Exec(ctx, "SELECT "+lock+"($1)", SyncLockID); err != nil {
		conn.Release()
		return fmt.Errorf("wallet lock: %w", err)
	}
	defer func() {
		// A session lock lives as long as the connection: if it cannot be
		// released, close the connection rather than return it to the pool
		// still holding the lock.
		if _, err := conn.Exec(context.Background(), "SELECT "+unlock+"($1)", SyncLockID); err != nil {
			_ = conn.Hijack().Close(context.Background())
			return
		}
		conn.Release()
	}()
	return fn(conn)
}
