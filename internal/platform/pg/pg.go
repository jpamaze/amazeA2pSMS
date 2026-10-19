// Package pg builds the PostgreSQL connection pool.
package pg

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// New opens a pool. Connections are made lazily, so a database that is down
// at boot shows up in readiness instead of crashing the service.
func New(ctx context.Context, url string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("pg: parse url: %w", err)
	}
	return pgxpool.NewWithConfig(ctx, cfg)
}

// Ping is a readiness check for a pool.
func Ping(p *pgxpool.Pool) func(context.Context) error {
	return func(ctx context.Context) error { return p.Ping(ctx) }
}
