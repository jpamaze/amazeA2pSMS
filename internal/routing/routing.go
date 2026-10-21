// Package routing chooses a vendor for a message from an in-memory copy of
// the routes, vendors, vendor costs and vendor deals tables, reloaded in the
// background (blueprint rule 6: config is never queried per message).
//
// A vendor deal replaces the vendor's cost on its destination while it runs.
// Until its committed volume has been routed, least-cost routing picks among
// vendors with unmet commitments first, so commitments are met before
// cheaper spot routes are used.
package routing

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/shopspring/decimal"

	"github.com/jpamaze/amazeA2pSMS/internal/billing/deals"
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

// VendorDeal is one row of vendor_deals.
type VendorDeal struct {
	ID, VendorID string
	MCC, MNC     string // MNC '*' for every network
	Cost         decimal.Decimal
	Commit       int64 // segments
	Starts, Ends time.Time
}

// Progress counts segments routed under each vendor deal. It is shared by
// successive tables and refreshed from Redis, where every router adds to it.
type Progress struct {
	mu     sync.Mutex
	routed map[string]int64
}

// Add counts n segments routed under deal id.
func (p *Progress) Add(id string, n int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.routed == nil {
		p.routed = map[string]int64{}
	}
	p.routed[id] += n
}

// Get returns segments routed under deal id.
func (p *Progress) Get(id string) int64 {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.routed[id]
}

// Set replaces the counts with totals across all routers.
func (p *Progress) Set(m map[string]int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.routed = m
}

// Table is an immutable snapshot of routing config.
type Table struct {
	rules    []Rule
	active   map[string]bool                       // vendor ID -> status active
	costs    map[string]map[string]decimal.Decimal // vendor -> "mcc|mnc" -> cost
	deals    map[string][]VendorDeal               // vendor -> deals not yet ended
	progress *Progress
	now      func() time.Time
	loadedAt time.Time
}

// NewTable builds a snapshot (used by tests and the loader).
func NewTable(rules []Rule, active map[string]bool, costs map[string]map[string]decimal.Decimal) *Table {
	r := append([]Rule(nil), rules...)
	sort.SliceStable(r, func(i, j int) bool { return r[i].Priority < r[j].Priority })
	return &Table{rules: r, active: active, costs: costs, now: time.Now, loadedAt: time.Now()}
}

// WithDeals adds vendor deals and their shared progress counter (nil counts
// nothing as routed yet). Call it before the table is in use.
func (t *Table) WithDeals(list []VendorDeal, p *Progress) *Table {
	t.deals = map[string][]VendorDeal{}
	for _, d := range list {
		t.deals[d.VendorID] = append(t.deals[d.VendorID], d)
	}
	t.progress = p
	return t
}

// Decision is the chosen vendor, its cost per segment and the vendor deal
// that set the cost, if any.
type Decision struct {
	VendorID string
	Cost     decimal.Decimal
	DealID   string
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
					d, _, _ := t.price(r.VendorID, mcc, mnc)
					return d, true
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
					d, _, _ := t.price(r.VendorID, mcc, mnc)
					return d, true
				}
				n -= r.Weight
			}
		}
	}
	return Decision{}, false
}

// price returns what vendor charges for the destination: the running deal's
// cost if there is one, else its cost list. open reports a deal whose
// commitment is not yet met; ok is false when the vendor has no price.
func (t *Table) price(vendor, mcc, mnc string) (d Decision, open, ok bool) {
	d.VendorID = vendor
	if deal := t.deal(vendor, mcc, mnc); deal != nil {
		d.Cost, d.DealID = deal.Cost, deal.ID
		return d, t.progress.Get(deal.ID) < deal.Commit, true
	}
	m := t.costs[vendor]
	if c, found := m[mcc+"|"+mnc]; found {
		d.Cost = c
		return d, false, true
	}
	d.Cost, ok = m[mcc+"|*"]
	return d, false, ok
}

// deal returns the vendor's deal running now for the destination; one for
// the exact network wins over a '*' deal.
func (t *Table) deal(vendor, mcc, mnc string) *VendorDeal {
	now := t.now()
	var best *VendorDeal
	for i := range t.deals[vendor] {
		d := &t.deals[vendor][i]
		if d.MCC != mcc || (d.MNC != "*" && d.MNC != mnc) || now.Before(d.Starts) || !now.Before(d.Ends) {
			continue
		}
		if best == nil || (best.MNC == "*" && d.MNC != "*") {
			best = d
		}
	}
	return best
}

// cheapest picks the lowest cost among vendors with an unmet deal
// commitment, or among all active vendors when there are none.
func (t *Table) cheapest(mcc, mnc string) (Decision, bool) {
	seen := map[string]bool{}
	vendors := make([]string, 0, len(t.costs)+len(t.deals))
	for _, m := range []map[string]bool{keys(t.costs), keys(t.deals)} {
		for v := range m {
			if !seen[v] {
				seen[v] = true
				vendors = append(vendors, v)
			}
		}
	}
	sort.Strings(vendors) // deterministic tie-break
	var best, bestOpen Decision
	found, foundOpen := false, false
	for _, v := range vendors {
		if !t.active[v] {
			continue
		}
		d, open, ok := t.price(v, mcc, mnc)
		if !ok {
			continue
		}
		if !found || d.Cost.LessThan(best.Cost) {
			best, found = d, true
		}
		if open && (!foundOpen || d.Cost.LessThan(bestOpen.Cost)) {
			bestOpen, foundOpen = d, true
		}
	}
	if foundOpen {
		return bestOpen, true
	}
	return best, found
}

func keys[V any](m map[string]V) map[string]bool {
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}

// Loader keeps a Table fresh from PostgreSQL, and vendor deal progress
// fresh from Redis.
type Loader struct {
	pool     *pgxpool.Pool
	rdb      redis.UniversalClient // nil: deal progress is local only
	log      *slog.Logger
	table    atomic.Pointer[Table]
	Progress *Progress
}

// NewLoader loads once and returns the loader. rdb may be nil.
func NewLoader(ctx context.Context, pool *pgxpool.Pool, rdb redis.UniversalClient, log *slog.Logger) (*Loader, error) {
	l := &Loader{pool: pool, rdb: rdb, log: log, Progress: &Progress{}}
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

	rows, err = l.pool.Query(ctx, `
		SELECT id, vendor_id, mcc, mnc, cost, commit_segments, starts_at, ends_at
		FROM vendor_deals WHERE status = 'active' AND ends_at > now()`)
	if err != nil {
		return nil, err
	}
	var vds []VendorDeal
	var ids []string
	for rows.Next() {
		var d VendorDeal
		if err := rows.Scan(&d.ID, &d.VendorID, &d.MCC, &d.MNC, &d.Cost, &d.Commit, &d.Starts, &d.Ends); err != nil {
			return nil, err
		}
		vds = append(vds, d)
		ids = append(ids, d.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if l.rdb != nil {
		if n, err := deals.Routed(ctx, l.rdb, ids); err != nil {
			l.log.Warn("vendor deal progress not refreshed", "err", err)
		} else {
			l.Progress.Set(n)
		}
	}
	return NewTable(rules, active, costs).WithDeals(vds, l.Progress), nil
}
