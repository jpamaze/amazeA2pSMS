// Package rater settles balance holds from status events and writes the
// charges to the PostgreSQL ledger in batches (blueprint rule 4).
//
// Redis settle/release is idempotent per hold. The ledger is exactly-once:
// each batch transaction records the last offset per partition it covered.
package rater

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jpamaze/amazeA2pSMS/internal/platform/metrics"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/jpamaze/amazeA2pSMS/internal/billing/wallet"
	"github.com/jpamaze/amazeA2pSMS/internal/domain"
)

// Rater settles holds and books charges.
type Rater struct {
	Wallets *wallet.Store
	PG      *pgxpool.Pool
	Log     *slog.Logger
}

// charged reports whether a status event means the customer pays: the
// vendor accepted at least one part.
func charged(ev domain.StatusEvent) bool {
	return ev.Status == domain.StatusSubmitted || (ev.Status == domain.StatusFailed && len(ev.VendorMsgIDs) > 0)
}

// released reports whether the hold is returned without charge.
func released(ev domain.StatusEvent) bool {
	return ev.Status == domain.StatusRejected || (ev.Status == domain.StatusFailed && len(ev.VendorMsgIDs) == 0)
}

type tp struct {
	topic     string
	partition int32
}

// Process handles one batch of sms.status records.
func (r *Rater) Process(ctx context.Context, recs []*kgo.Record) error {
	// 1. Redis: settle or release every hold of every paying account (idempotent).
	type charge struct {
		rec   *kgo.Record
		acct  string
		price decimal.Decimal
	}
	var charges []charge
	for _, rec := range recs {
		var ev domain.StatusEvent
		if err := json.Unmarshal(rec.Value, &ev); err != nil {
			r.Log.Error("bad status record; skipping", "offset", rec.Offset, "err", err)
			continue
		}
		levels := ev.Charges
		if len(levels) == 0 && ev.HoldID != "" { // events from before multi-level charging
			levels = []domain.Charge{{AccountID: ev.AccountID, HoldID: ev.HoldID, Amount: ev.Price}}
		}
		switch {
		case charged(ev):
			for _, l := range levels {
				if l.HoldID != "" && l.Amount.IsPositive() {
					_, err := r.Wallets.Settle(ctx, l.AccountID, l.HoldID, wallet.FromDecimal(l.Amount))
					if err != nil && !errors.Is(err, wallet.ErrUnknownHold) {
						return err
					}
				}
				if l.Amount.IsPositive() {
					charges = append(charges, charge{rec: rec, acct: l.AccountID, price: l.Amount})
				}
			}
		case released(ev):
			for _, l := range levels {
				if l.HoldID == "" || !l.Amount.IsPositive() {
					continue
				}
				if err := r.Wallets.Release(ctx, l.AccountID, l.HoldID); err != nil && !errors.Is(err, wallet.ErrUnknownHold) {
					return err
				}
			}
		}
	}

	// 2. PostgreSQL: one ledger entry per account for records not yet booked.
	metrics.Charged.Add(float64(len(charges)))
	return pgx.BeginFunc(ctx, r.PG, func(tx pgx.Tx) error {
		last := map[tp]int64{}
		high := map[tp]int64{}
		for _, rec := range recs {
			k := tp{rec.Topic, rec.Partition}
			if _, ok := last[k]; !ok {
				var off int64 = -1
				err := tx.QueryRow(ctx, `SELECT last_offset FROM rater_offsets WHERE topic = $1 AND partition = $2 FOR UPDATE`,
					k.topic, k.partition).Scan(&off)
				if err != nil && !errors.Is(err, pgx.ErrNoRows) {
					return err
				}
				last[k] = off
			}
			high[k] = max(high[k], rec.Offset)
		}

		sums := map[string]decimal.Decimal{}
		counts := map[string]int{} // messages charged per account
		for _, c := range charges {
			if c.rec.Offset <= last[tp{c.rec.Topic, c.rec.Partition}] {
				continue // already booked by an earlier run of this batch
			}
			sums[c.acct] = sums[c.acct].Add(c.price)
			counts[c.acct]++
		}
		batch := uuid.NewString()
		for acct, sum := range sums {
			var bal decimal.Decimal
			if err := tx.QueryRow(ctx, `UPDATE wallets SET balance = balance - $2, updated_at = now()
				WHERE account_id = $1 RETURNING balance`, acct, sum).Scan(&bal); err != nil {
				return fmt.Errorf("rater: debit %s: %w", acct, err)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO ledger_entries
				(account_id, kind, amount, balance_after, reference, idempotency_key, created_by, quantity)
				VALUES ($1, 'charge', $2, $3, $4, $5, 'rater', $6)`,
				acct, sum.Neg(), bal, fmt.Sprintf("%d messages", counts[acct]), "rater:"+batch+":"+acct, counts[acct]); err != nil {
				return err
			}
		}
		for k, off := range high {
			if off <= last[k] {
				continue
			}
			if _, err := tx.Exec(ctx, `INSERT INTO rater_offsets (topic, partition, last_offset) VALUES ($1, $2, $3)
				ON CONFLICT (topic, partition) DO UPDATE SET last_offset = EXCLUDED.last_offset, updated_at = now()`,
				k.topic, k.partition, off); err != nil {
				return err
			}
		}
		return nil
	})
}
