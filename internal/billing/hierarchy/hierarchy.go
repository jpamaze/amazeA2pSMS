// Package hierarchy resolves who pays for a message: the sending account and
// every reseller above it (the platform account itself is never charged).
// The chain is cached briefly so the message path does not query PostgreSQL
// per message.
package hierarchy

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrInactive means the account or a reseller above it is suspended or closed.
var ErrInactive = errors.New("hierarchy: account or its reseller is not active")

// Chain returns the paying accounts, sender first, then each reseller upward.
type Chain interface {
	Chain(ctx context.Context, accountID string) ([]string, error)
}

// Cached is a PostgreSQL-backed Chain with a TTL cache.
type Cached struct {
	PG  *pgxpool.Pool
	TTL time.Duration

	mu    sync.RWMutex
	cache map[string]entry
}

type entry struct {
	chain   []string
	err     error
	expires time.Time
}

// Chain implements Chain.
func (c *Cached) Chain(ctx context.Context, accountID string) ([]string, error) {
	now := time.Now()
	c.mu.RLock()
	e, ok := c.cache[accountID]
	c.mu.RUnlock()
	if ok && now.Before(e.expires) {
		return e.chain, e.err
	}
	rows, err := c.PG.Query(ctx, `
		WITH RECURSIVE up AS (
			SELECT id, parent_id, role, status, 0 AS depth FROM accounts WHERE id = $1
			UNION ALL
			SELECT a.id, a.parent_id, a.role, a.status, up.depth + 1
			FROM accounts a JOIN up ON a.id = up.parent_id
			WHERE up.depth < 10
		)
		SELECT id::text, role, status FROM up ORDER BY depth`, accountID)
	if err != nil {
		return nil, fmt.Errorf("hierarchy: %w", err)
	}
	defer rows.Close()
	var chain []string
	var chainErr error
	for rows.Next() {
		var id, role, status string
		if err := rows.Scan(&id, &role, &status); err != nil {
			return nil, err
		}
		if role == "platform" {
			break
		}
		if status != "active" {
			chainErr = ErrInactive
		}
		chain = append(chain, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(chain) == 0 {
		chainErr = ErrInactive // unknown account
	}
	c.mu.Lock()
	if c.cache == nil {
		c.cache = map[string]entry{}
	}
	c.cache[accountID] = entry{chain: chain, err: chainErr, expires: now.Add(c.TTL)}
	c.mu.Unlock()
	return chain, chainErr
}
