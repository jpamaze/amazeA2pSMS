package dlt

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Mapped is what a vendor receipt error code means for the customer.
type Mapped struct {
	Reason       string // dlt_template, dlt_dnd, ... or another label
	CustomerCode string // err: value in the customer's receipt
}

// ErrorMap translates vendor receipt error codes (vendor_error_codes),
// kept in memory and reloaded in the background.
type ErrorMap struct {
	PG  *pgxpool.Pool
	Log *slog.Logger
	m   atomic.Pointer[map[string]Mapped] // "vendor|code"; vendor "" = every vendor
}

// Lookup returns the vendor's own mapping for code, else the one for every
// vendor.
func (e *ErrorMap) Lookup(vendor, code string) (Mapped, bool) {
	m := e.m.Load()
	if m == nil || code == "" {
		return Mapped{}, false
	}
	if v, ok := (*m)[vendor+"|"+code]; ok {
		return v, true
	}
	v, ok := (*m)["|"+code]
	return v, ok
}

// Load reads the table once.
func (e *ErrorMap) Load(ctx context.Context) error {
	rows, err := e.PG.Query(ctx, `SELECT coalesce(vendor_id, ''), code, reason, customer_code FROM vendor_error_codes`)
	if err != nil {
		return err
	}
	defer rows.Close()
	m := map[string]Mapped{}
	for rows.Next() {
		var v, code string
		var x Mapped
		if err := rows.Scan(&v, &code, &x.Reason, &x.CustomerCode); err != nil {
			return err
		}
		m[v+"|"+code] = x
	}
	if err := rows.Err(); err != nil {
		return err
	}
	e.m.Store(&m)
	return nil
}

// Run reloads every interval until ctx ends, keeping the last good map.
func (e *ErrorMap) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := e.Load(ctx); err != nil && e.Log != nil {
				e.Log.Warn("vendor error codes not reloaded", "err", err)
			}
		}
	}
}
