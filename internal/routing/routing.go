// Package routing chooses a vendor for a message from an in-memory copy of
// the routes, vendors and vendor costs tables, reloaded in the background
// (blueprint rule 6: config is never queried per message).
package routing

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"sort"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

// ErrNoRoute means no active vendor can take the message.
var ErrNoRoute = errors.New("routing: no route")

// Rule is one row of the routes table.
type Rule struct {
	AccountID string // empty = platform default
	MCC, MNC  string
	Mode      string // lcr | priority | weighted
	VendorID  string
	Priority  int
	Weight    int
}

// Table is an immutable snapshot of routing config.
type Table struct {
	rules    []Rule
	active   map[string]bool                       // vendor ID -> status active
	costs    map[string]map[string]decimal.Decimal // vendor -> "mcc|mnc" -> cost
	loadedAt time.Time
}

// NewTable builds a snapshot (used by tests and the loader).
func NewTable(rules []Rule, active map[string]bool, costs map[string]map[string]decimal.Decimal) *Table {
	r := append([]Rule(nil), rules...)
	sort.SliceStable(r, func(i, j int) bool { return r[i].Priority < r[j].Priority })
	return &Table{rules: r, active: active, costs: costs, loadedAt: time.Now()}
}

// Decision is the chosen vendor and its cost per segment.
type Decision struct {
	VendorID string
	Cost     decimal.Decimal
}

// Choose picks a vendor. Account-specific rules win over platform defaults;
// within a set, lower priority numbers are tried first.
func (t *Table) Choose(accountID, mcc, mnc string) (Decision, error) {
	for _, scope := range []string{accountID, ""} {
		if scope == "" && accountID == "" {
			continue
		}
		var matched []Rule
		for _, r := range t.rules {
			if r.AccountID == scope && r.MCC == mcc && (r.MNC == "*" || r.MNC == mnc) {
				matched = append(matched, r)
			}
		}
		if d, ok := t.pick(matched, mcc, mnc); ok {
			return d, nil
		}
	}
	return Decision{}, ErrNoRoute
}

func (t *Table) pick(rules []Rule, mcc, mnc string) (Decision, bool) {
	for i := 0; i < len(rules); {
		j := i
		for j < len(rules) && rules[j].Priority == rules[i].Priority {
			j++
		}
		group := rules[i:j]
		i = j

		var weighted []Rule
		for _, r := range group {
			switch r.Mode {
			case "lcr":
				if d, ok := t.cheapest(mcc, mnc); ok {
					return d, true
				}
			case "priority":
				if t.active[r.VendorID] {
					if c, ok := t.cost(r.VendorID, mcc, mnc); ok {
						return Decision{VendorID: r.VendorID, Cost: c}, true
					}
					return Decision{VendorID: r.VendorID}, true
				}
			case "weighted":
				if t.active[r.VendorID] {
					weighted = append(weighted, r)
				}
			}
		}
		if len(weighted) > 0 {
			total := 0
			for _, r := range weighted {
				total += r.Weight
			}
			n := rand.IntN(total)
			for _, r := range weighted {
				if n < r.Weight {
					c, _ := t.cost(r.VendorID, mcc, mnc)
					return Decision{VendorID: r.VendorID, Cost: c}, true
				}
				n -= r.Weight
			}
		}
	}
	return Decision{}, false
}

func (t *Table) cost(vendor, mcc, mnc string) (decimal.Decimal, bool) {
	m := t.costs[vendor]
	if c, ok := m[mcc+"|"+mnc]; ok {
		return c, true
	}
	c, ok := m[mcc+"|*"]
	return c, ok
}

func (t *Table) cheapest(mcc, mnc string) (Decision, bool) {
	var best Decision
	found := false
	vendors := make([]string, 0, len(t.costs))
	for v := range t.costs {
		vendors = append(vendors, v)
	}
	sort.Strings(vendors) // deterministic tie-break
	for _, v := range vendors {
		if !t.active[v] {
			continue
		}
		c, ok := t.cost(v, mcc, mnc)
		if ok && (!found || c.LessThan(best.Cost)) {
			best, found = Decision{VendorID: v, Cost: c}, true
		}
	}
	return best, found
}

// Loader keeps a Table fresh from PostgreSQL.
type Loader struct {
	pool  *pgxpool.Pool
	log   *slog.Logger
	table atomic.Pointer[Table]
}

// NewLoader loads once and returns the loader.
func NewLoader(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger) (*Loader, error) {
	l := &Loader{pool: pool, log: log}
	t, err := l.load(ctx)
	if err != nil {
		return nil, err
	}
	l.table.Store(t)
	return l, nil
}

// Table returns the current snapshot.
func (l *Loader) Table() *Table { return l.table.Load() }

// Run reloads every interval until ctx is cancelled. A failed reload keeps
// the last good table.
func (l *Loader) Run(ctx context.Context, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			t, err := l.load(ctx)
			if err != nil {
				l.log.Warn("routing reload failed; keeping last table", "err", err)
				continue
			}
			l.table.Store(t)
		}
	}
}

func (l *Loader) load(ctx context.Context) (*Table, error) {
	rows, err := l.pool.Query(ctx, `
		SELECT coalesce(account_id::text, ''), mcc, mnc, mode, coalesce(vendor_id, ''), priority, weight
		FROM routes WHERE enabled`)
	if err != nil {
		return nil, err
	}
	var rules []Rule
	for rows.Next() {
		var r Rule
		if err := rows.Scan(&r.AccountID, &r.MCC, &r.MNC, &r.Mode, &r.VendorID, &r.Priority, &r.Weight); err != nil {
			return nil, err
		}
		rules = append(rules, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	active := map[string]bool{}
	rows, err = l.pool.Query(ctx, `SELECT id, status = 'active' FROM vendors`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		var ok bool
		if err := rows.Scan(&id, &ok); err != nil {
			return nil, err
		}
		active[id] = ok
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	costs := map[string]map[string]decimal.Decimal{}
	rows, err = l.pool.Query(ctx, `
		SELECT DISTINCT ON (vendor_id, mcc, mnc) vendor_id, mcc, mnc, cost
		FROM vendor_costs WHERE valid_from <= now() AND category = '*'
		ORDER BY vendor_id, mcc, mnc, valid_from DESC`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var v, mcc, mnc string
		var c decimal.Decimal
		if err := rows.Scan(&v, &mcc, &mnc, &c); err != nil {
			return nil, err
		}
		if costs[v] == nil {
			costs[v] = map[string]decimal.Decimal{}
		}
		costs[v][mcc+"|"+mnc] = c
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return NewTable(rules, active, costs), nil
}
