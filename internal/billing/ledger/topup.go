// Package ledger books money movements that originate outside the message
// path (top-ups and adjustments) in PostgreSQL and mirrors them to Redis.
package ledger

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/jpamaze/amazeA2pSMS/internal/billing/wallet"
)

// ErrDuplicate means the idempotency key was already used; the original
// entry stands and nothing changed.
var ErrDuplicate = errors.New("ledger: duplicate idempotency key")

// TopUp is one credit to an account's wallet.
type TopUp struct {
	AccountID      string
	Amount         decimal.Decimal
	Reference      string // payment reference, invoice number
	IdempotencyKey string
	Actor          string
}

// Service books top-ups.
type Service struct {
	PG      *pgxpool.Pool
	Wallets *wallet.Store
}

// TopUp credits the wallet in PostgreSQL (with a ledger entry) and then in
// Redis. It returns the database balance after the top-up.
func (s *Service) TopUp(ctx context.Context, t TopUp) (decimal.Decimal, error) {
	if !t.Amount.IsPositive() {
		return decimal.Zero, fmt.Errorf("ledger: top-up amount must be positive")
	}
	if t.IdempotencyKey == "" || t.Actor == "" {
		return decimal.Zero, fmt.Errorf("ledger: idempotency key and actor are required")
	}
	var bal, limit decimal.Decimal
	err := pgx.BeginFunc(ctx, s.PG, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `UPDATE wallets SET balance = balance + $2, updated_at = now()
			WHERE account_id = $1 RETURNING balance, credit_limit`, t.AccountID, t.Amount).Scan(&bal, &limit); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO ledger_entries
			(account_id, kind, amount, balance_after, reference, idempotency_key, created_by)
			VALUES ($1, 'topup', $2, $3, $4, $5, $6)`,
			t.AccountID, t.Amount, bal, t.Reference, "topup:"+t.IdempotencyKey, t.Actor)
		return err
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return decimal.Zero, ErrDuplicate
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return decimal.Zero, fmt.Errorf("ledger: account %s has no wallet", t.AccountID)
	}
	if err != nil {
		return decimal.Zero, err
	}
	if _, err := s.Wallets.ApplyTopUp(ctx, t.AccountID, wallet.FromDecimal(t.Amount),
		wallet.FromDecimal(bal), wallet.FromDecimal(limit)); err != nil {
		return bal, fmt.Errorf("ledger: top-up saved but not yet spendable (Redis): %w", err)
	}
	return bal, nil
}
