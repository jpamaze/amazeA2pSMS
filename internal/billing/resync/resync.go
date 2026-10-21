// Package resync rebuilds the Redis copy of every wallet from PostgreSQL,
// the system of record. It is the repair after anything that can lose Redis
// writes: a Redis failover (replication is asynchronous, so the newest
// writes may not have reached the promoted replica), a restore, or a top-up
// whose Redis step failed.
//
// It runs holding the wallet sync lock exclusively, so no rater batch,
// top-up or transfer is half done while it reads PostgreSQL and writes
// Redis; messages keep being accepted meanwhile (holds are recounted
// atomically). Holds are kept, except stale ones: older than StaleAfter and
// for a message that is no longer waiting to be sent (or whose state is
// gone). Their charge, if any, is already in the ledger through the rater.
package resync

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/shopspring/decimal"

	"github.com/jpamaze/amazeA2pSMS/internal/billing/wallet"
	"github.com/jpamaze/amazeA2pSMS/internal/domain"
	"github.com/jpamaze/amazeA2pSMS/internal/msgstore"
)

// Options tune a run.
type Options struct {
	DryRun     bool          // report only
	StaleAfter time.Duration // minimum hold age before it may be dropped (default 1 h)
	Now        func() time.Time
}

// Drift is one wallet that differed.
type Drift struct {
	AccountID             string
	RedisBalance, Balance wallet.Money
	RedisLimit, Limit     wallet.Money
	HeldBefore, HeldAfter wallet.Money
	StaleHolds            int
	StaleAmount           wallet.Money
}

// Report sums up a run.
type Report struct {
	Wallets int
	Drifts  []Drift
	Took    time.Duration
}

// Run checks (and unless DryRun, repairs) every wallet.
func Run(ctx context.Context, pool *pgxpool.Pool, rdb redis.UniversalClient, o Options) (Report, error) {
	if o.StaleAfter == 0 {
		o.StaleAfter = time.Hour
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	start := time.Now()
	ws := wallet.New(rdb)
	var rep Report
	err := wallet.Exclusive(ctx, pool, func(conn *pgxpool.Conn) error {
		rows, err := conn.Query(ctx, `SELECT account_id::text, balance, credit_limit FROM wallets ORDER BY account_id`)
		if err != nil {
			return err
		}
		type pgWallet struct {
			id         string
			bal, limit decimal.Decimal
		}
		var list []pgWallet
		for rows.Next() {
			var w pgWallet
			if err := rows.Scan(&w.id, &w.bal, &w.limit); err != nil {
				rows.Close()
				return err
			}
			list = append(list, w)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		rep.Wallets = len(list)
		for _, w := range list {
			d, changed, err := check(ctx, rdb, ws, w.id, wallet.FromDecimal(w.bal), wallet.FromDecimal(w.limit), o)
			if err != nil {
				return fmt.Errorf("wallet %s: %w", w.id, err)
			}
			if changed {
				rep.Drifts = append(rep.Drifts, d)
			}
		}
		return nil
	})
	rep.Took = time.Since(start)
	return rep, err
}

func check(ctx context.Context, rdb redis.UniversalClient, ws *wallet.Store, acct string, bal, limit wallet.Money, o Options) (Drift, bool, error) {
	snap, err := ws.Get(ctx, acct)
	if err != nil {
		return Drift{}, false, err
	}
	holds, err := ws.Holds(ctx, acct)
	if err != nil {
		return Drift{}, false, err
	}
	d := Drift{AccountID: acct, RedisBalance: snap.Balance, Balance: bal, RedisLimit: snap.CreditLimit, Limit: limit, HeldBefore: snap.Held}
	var sum wallet.Money
	var stale []string
	for id, amt := range holds {
		sum += amt
		if isStale(ctx, rdb, id, o) {
			stale = append(stale, id)
			d.StaleHolds++
			d.StaleAmount += amt
		}
	}
	d.HeldAfter = sum - d.StaleAmount
	changed := snap.Balance != bal || snap.CreditLimit != limit || snap.Held != sum || len(stale) > 0
	if !changed || o.DryRun {
		return d, changed, nil
	}
	before, after, _, err := ws.Repair(ctx, acct, bal, limit, stale)
	if err != nil {
		return d, true, err
	}
	d.HeldBefore, d.HeldAfter = before, after
	return d, true, nil
}

// isStale reports whether a hold (named by its message's ID, a UUIDv7 that
// carries its acceptance time) is old and no longer waiting to be sent.
func isStale(ctx context.Context, rdb redis.UniversalClient, holdID string, o Options) bool {
	u, err := uuid.Parse(holdID)
	if err != nil || u.Version() != 7 {
		return false
	}
	sec, nsec := u.Time().UnixTime()
	if o.Now().Sub(time.Unix(sec, nsec)) < o.StaleAfter {
		return false
	}
	st, err := msgstore.New(rdb).Status(ctx, holdID)
	if errors.Is(err, msgstore.ErrNotFound) {
		return true // state gone (expired or lost)
	}
	return err == nil && st != domain.StatusAccepted
}
