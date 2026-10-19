// Package pricing looks up what a customer pays per SMS segment.
//
// Prices come from the account's rate card (rate_card_entries) and are
// cached in memory, so the message path does not query PostgreSQL per
// message (blueprint rule 6).
package pricing

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

// ErrNoPrice means the account has no rate for this destination.
var ErrNoPrice = errors.New("pricing: no rate for destination")

// Pricer returns the price per segment for an account and destination network.
type Pricer interface {
	Price(ctx context.Context, accountID, mcc, mnc, category string) (decimal.Decimal, error)
}

// Cached is a PostgreSQL-backed Pricer with a TTL cache.
type Cached struct {
	pool *pgxpool.Pool
	ttl  time.Duration

	mu    sync.RWMutex
	cache map[string]entry
}

type entry struct {
	price   decimal.Decimal
	err     error
	expires time.Time
}

// NewCached returns a Pricer that caches each answer (including "no price") for ttl.
func NewCached(pool *pgxpool.Pool, ttl time.Duration) *Cached {
	return &Cached{pool: pool, ttl: ttl, cache: map[string]entry{}}
}

const query = `
SELECT e.price
FROM accounts a
JOIN rate_card_entries e ON e.rate_card_id = a.rate_card_id
WHERE a.id = $1 AND e.mcc = $2 AND e.mnc IN ($3, '*') AND e.category IN ($4, '*')
  AND e.valid_from <= now()
ORDER BY (e.mnc = '*'), (e.category = '*'), e.valid_from DESC
LIMIT 1`

// Price implements Pricer.
func (c *Cached) Price(ctx context.Context, accountID, mcc, mnc, category string) (decimal.Decimal, error) {
	if category == "" {
		category = "*"
	}
	key := accountID + "|" + mcc + "|" + mnc + "|" + category
	now := time.Now()
	c.mu.RLock()
	e, ok := c.cache[key]
	c.mu.RUnlock()
	if ok && now.Before(e.expires) {
		return e.price, e.err
	}

	var price decimal.Decimal
	err := c.pool.QueryRow(ctx, query, accountID, mcc, mnc, category).Scan(&price)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNoPrice
	} else if err != nil {
		// Do not cache infrastructure errors.
		return decimal.Zero, fmt.Errorf("pricing: %w", err)
	}
	c.mu.Lock()
	c.cache[key] = entry{price: price, err: err, expires: now.Add(c.ttl)}
	c.mu.Unlock()
	return price, err
}
