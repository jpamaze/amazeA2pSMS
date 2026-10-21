// Package deals applies customer deals (a special price for one account on a
// destination for a period, optionally capped by volume) and counts traffic
// routed under vendor deals.
//
// Deals are read from PostgreSQL and cached, so the message path never
// queries the database per message. A capped deal's volume is reserved in
// Redis per message, the same way as balance holds: the reservation is kept
// when the message is charged and given back when it is released, and both
// are idempotent per message ID.
package deals

import (
	"context"
	_ "embed"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/shopspring/decimal"
)

// Deal is an active customer deal.
type Deal struct {
	ID     string
	MNC    string // '*' for every network
	Price  decimal.Decimal
	Cap    int64 // segments; 0 = no cap
	Used   int64 // charged segments when loaded (seeds the Redis counter)
	Starts time.Time
	Ends   time.Time
}

// Book finds and reserves customer deals.
type Book struct {
	PG    *pgxpool.Pool
	Redis redis.UniversalClient
	TTL   time.Duration // cache lifetime (default 1 minute)

	mu    sync.RWMutex
	cache map[string]cached
	full  map[string]time.Time // capped deals found used up, until this time
}

type cached struct {
	deals   []Deal
	expires time.Time
}

// Find returns the deal in effect now for an account and destination, or
// nil. A deal for the exact network wins over a '*' deal; among equals the
// one that started last wins.
func (b *Book) Find(ctx context.Context, accountID, mcc, mnc string) (*Deal, error) {
	list, err := b.load(ctx, accountID, mcc)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	var best *Deal
	for i := range list {
		d := &list[i]
		if now.Before(d.Starts) || !now.Before(d.Ends) || (d.MNC != "*" && d.MNC != mnc) {
			continue
		}
		if best == nil || (d.MNC != "*" && best.MNC == "*") ||
			((d.MNC == "*") == (best.MNC == "*") && d.Starts.After(best.Starts)) {
			best = d
		}
	}
	return best, nil
}

func (b *Book) load(ctx context.Context, accountID, mcc string) ([]Deal, error) {
	key := accountID + "|" + mcc
	now := time.Now()
	b.mu.RLock()
	c, ok := b.cache[key]
	b.mu.RUnlock()
	if ok && now.Before(c.expires) {
		return c.deals, nil
	}
	rows, err := b.PG.Query(ctx, `SELECT id, mnc, price, coalesce(cap_segments, 0), used_segments, starts_at, ends_at
		FROM customer_deals WHERE account_id = $1 AND mcc = $2 AND status = 'active' AND ends_at > now()`, accountID, mcc)
	if err != nil {
		return nil, fmt.Errorf("deals: %w", err)
	}
	defer rows.Close()
	var list []Deal
	for rows.Next() {
		var d Deal
		if err := rows.Scan(&d.ID, &d.MNC, &d.Price, &d.Cap, &d.Used, &d.Starts, &d.Ends); err != nil {
			return nil, fmt.Errorf("deals: %w", err)
		}
		list = append(list, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("deals: %w", err)
	}
	b.mu.Lock()
	if b.cache == nil {
		b.cache = map[string]cached{}
	}
	b.cache[key] = cached{deals: list, expires: now.Add(b.ttl())}
	b.mu.Unlock()
	return list, nil
}

func (b *Book) ttl() time.Duration {
	if b.TTL == 0 {
		return time.Minute
	}
	return b.TTL
}

var (
	//go:embed reserve.lua
	reserveSrc    string
	reserveScript = redis.NewScript(reserveSrc)
	//go:embed release.lua
	releaseSrc    string
	releaseScript = redis.NewScript(releaseSrc)
)

func usedKey(dealID string) string     { return fmt.Sprintf("deal:{%s}", dealID) }
func reservedKey(dealID string) string { return fmt.Sprintf("deal:{%s}:res", dealID) }

// Keep counters a week past the deal's end for reporting.
const keep = 7 * 24 * time.Hour

// Reserve takes segments of a capped deal's volume for msgID. It returns
// false when the remaining volume does not cover the whole message. A deal
// found used up is not asked again until the cache refreshes, so senders on
// a finished deal cost no extra Redis call per message (volume released by
// failed messages becomes usable again within the cache TTL).
func (b *Book) Reserve(ctx context.Context, d *Deal, msgID string, segments int) (bool, error) {
	now := time.Now()
	b.mu.RLock()
	until, full := b.full[d.ID]
	b.mu.RUnlock()
	if full && now.Before(until) {
		return false, nil
	}
	ok, remaining, err := Reserve(ctx, b.Redis, d, msgID, segments)
	if err == nil && remaining <= 0 {
		b.mu.Lock()
		if b.full == nil {
			b.full = map[string]time.Time{}
		}
		b.full[d.ID] = now.Add(b.ttl())
		b.mu.Unlock()
	}
	return ok, err
}

// Reserve takes segments of a capped deal's volume for msgID and returns the
// segments left afterwards; ok is false when the remaining volume does not
// cover the whole message.
func Reserve(ctx context.Context, rdb redis.UniversalClient, d *Deal, msgID string, segments int) (ok bool, remaining int64, err error) {
	res, err := reserveScript.Run(ctx, rdb, []string{usedKey(d.ID), reservedKey(d.ID)},
		msgID, segments, d.Cap, d.Used, d.Ends.Add(keep).UnixMilli()).Int64Slice()
	if err != nil {
		return false, 0, fmt.Errorf("deals: reserve: %w", err)
	}
	return res[0] == 1, res[1], nil
}

// Release gives a message's reserved volume back (message not charged).
func Release(ctx context.Context, rdb redis.UniversalClient, dealID, msgID string) error {
	return releaseScript.Run(ctx, rdb, []string{usedKey(dealID), reservedKey(dealID)}, msgID).Err()
}

// Op settles (keeps) or releases (gives back) one message's reservation.
type Op struct {
	DealID, MsgID string
	Release       bool
}

// Apply runs many settles and releases in one round trip (the rater does
// this per batch).
func Apply(ctx context.Context, rdb redis.UniversalClient, ops []Op) error {
	if len(ops) == 0 {
		return nil
	}
	if err := releaseScript.Load(ctx, rdb).Err(); err != nil {
		return err
	}
	_, err := rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
		for _, o := range ops {
			if o.Release {
				p.EvalSha(ctx, releaseScript.Hash(), []string{usedKey(o.DealID), reservedKey(o.DealID)}, o.MsgID)
			} else {
				p.HDel(ctx, reservedKey(o.DealID), o.MsgID)
			}
		}
		return nil
	})
	return err
}

// Used returns the live used volume (charged plus in flight) of capped
// deals; deals never reserved are missing from the map.
func Used(ctx context.Context, rdb redis.UniversalClient, ids []string) (map[string]int64, error) {
	return counters(ctx, rdb, ids, usedKey, "used")
}

func vendorKey(dealID string) string { return fmt.Sprintf("vdeal:{%s}", dealID) }

// AddRouted counts segments routed under vendor deals (deal ID -> segments).
func AddRouted(ctx context.Context, rdb redis.UniversalClient, n map[string]int64) error {
	if len(n) == 0 {
		return nil
	}
	_, err := rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
		for id, v := range n {
			p.HIncrBy(ctx, vendorKey(id), "routed", v)
		}
		return nil
	})
	return err
}

// Routed returns segments routed so far under each vendor deal.
func Routed(ctx context.Context, rdb redis.UniversalClient, ids []string) (map[string]int64, error) {
	return counters(ctx, rdb, ids, vendorKey, "routed")
}

func counters(ctx context.Context, rdb redis.UniversalClient, ids []string, key func(string) string, field string) (map[string]int64, error) {
	out := make(map[string]int64, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	cmds := make([]*redis.StringCmd, len(ids))
	_, err := rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
		for i, id := range ids {
			cmds[i] = p.HGet(ctx, key(id), field)
		}
		return nil
	})
	if err != nil && err != redis.Nil {
		return nil, err
	}
	for i, c := range cmds {
		if v, err := c.Int64(); err == nil {
			out[ids[i]] = v
		}
	}
	return out, nil
}
