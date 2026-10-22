package ledger

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/jpamaze/amazeA2pSMS/internal/billing/wallet"
)

// Transfer moves money from one wallet to another (a reseller funding a
// sub-account).
type Transfer struct {
	From, To       string
	Amount         decimal.Decimal
	Reference      string
	IdempotencyKey string
	Actor          string
}

// Transfer debits From in Redis first (atomically, only if its available
// balance covers the amount, so a reseller cannot hand out money it is
// spending on in-flight messages), books both sides in PostgreSQL, then
// credits To in Redis. A failed or duplicate booking refunds the debit.
func (s *Service) Transfer(ctx context.Context, t Transfer) (decimal.Decimal, error) {
	return s.move(ctx, t, "transfer_out", "transfer_in", "transfer:")
}

// Refund credits an approved dispute to To. With From empty the platform
// pays (a 'refund' entry); otherwise that reseller's wallet pays, like a
// transfer ('refund_given' on From, 'refund' on To).
func (s *Service) Refund(ctx context.Context, t Transfer) (decimal.Decimal, error) {
	if t.From == "" {
		return s.credit(ctx, "refund", TopUp{AccountID: t.To, Amount: t.Amount, Reference: t.Reference,
			IdempotencyKey: t.IdempotencyKey, Actor: t.Actor})
	}
	return s.move(ctx, t, "refund_given", "refund", "refund:")
}

func (s *Service) move(ctx context.Context, t Transfer, outKind, inKind, keyPrefix string) (bal decimal.Decimal, err error) {
	err = wallet.Shared(ctx, s.PG, func(conn *pgxpool.Conn) error {
		bal, err = s.moveLocked(ctx, conn, t, outKind, inKind, keyPrefix)
		return err
	})
	return bal, err
}

func (s *Service) moveLocked(ctx context.Context, conn *pgxpool.Conn, t Transfer, outKind, inKind, keyPrefix string) (decimal.Decimal, error) {
	if !t.Amount.IsPositive() {
		return decimal.Zero, fmt.Errorf("ledger: transfer amount must be positive")
	}
	if t.From == t.To || t.IdempotencyKey == "" || t.Actor == "" {
		return decimal.Zero, fmt.Errorf("ledger: distinct accounts, idempotency key and actor are required")
	}
	var fromBal, fromLimit decimal.Decimal
	if err := conn.QueryRow(ctx, `SELECT balance, credit_limit FROM wallets WHERE account_id = $1`, t.From).Scan(&fromBal, &fromLimit); err != nil {
		return decimal.Zero, fmt.Errorf("ledger: source wallet: %w", err)
	}
	amt := wallet.FromDecimal(t.Amount)
	if _, err := s.Wallets.Debit(ctx, t.From, amt, wallet.FromDecimal(fromBal), wallet.FromDecimal(fromLimit)); err != nil {
		return decimal.Zero, err
	}
	refund := func() {
		bg, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.Wallets.Credit(bg, t.From, amt)
	}

	var toBal, toLimit decimal.Decimal
	err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		var outBal decimal.Decimal
		if err := tx.QueryRow(ctx, `UPDATE wallets SET balance = balance - $2, updated_at = now()
			WHERE account_id = $1 RETURNING balance`, t.From, t.Amount).Scan(&outBal); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `UPDATE wallets SET balance = balance + $2, updated_at = now()
			WHERE account_id = $1 RETURNING balance, credit_limit`, t.To, t.Amount).Scan(&toBal, &toLimit); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO ledger_entries
			(account_id, kind, amount, balance_after, reference, idempotency_key, created_by, counterparty) VALUES
			($1, $11, $3, $4, $5, $6, $7, $2),
			($2, $12, $8, $9, $5, $10, $7, $1)`,
			// Keys are scoped to the paying account: another account reusing
			// the same key must not collide with this transfer.
			t.From, t.To, t.Amount.Neg(), outBal, t.Reference, keyPrefix+t.From+":"+t.IdempotencyKey+":out", t.Actor,
			t.Amount, toBal, keyPrefix+t.From+":"+t.IdempotencyKey+":in", outKind, inKind)
		return err
	})
	var pgErr *pgconn.PgError
	switch {
	case errors.As(err, &pgErr) && pgErr.Code == "23505":
		refund()
		return decimal.Zero, ErrDuplicate
	case errors.Is(err, pgx.ErrNoRows):
		refund()
		return decimal.Zero, fmt.Errorf("ledger: destination wallet not found")
	case err != nil:
		refund()
		return decimal.Zero, err
	}
	if _, err := s.Wallets.ApplyTopUp(ctx, t.To, amt, wallet.FromDecimal(toBal), wallet.FromDecimal(toLimit)); err != nil {
		return toBal, fmt.Errorf("ledger: transfer booked but not yet spendable by the recipient (Redis): %w", err)
	}
	return toBal, nil
}
