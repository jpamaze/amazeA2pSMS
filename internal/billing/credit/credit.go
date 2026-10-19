// Package credit changes an account's credit limit.
//
// The database is the authority: its trigger enforces that a reseller gives
// sub-accounts credit only within its own available balance, and logs every
// change with who made it and why. After the commit, the hot copy in Redis is
// updated so the change takes effect for the next hold.
package credit

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/jpamaze/amazeA2pSMS/internal/billing/wallet"
)

var (
	// ErrResellerLimit means the reseller's available balance cannot cover the new limit.
	ErrResellerLimit = errors.New("credit: reseller available balance does not cover sub-account credit limits")
	// ErrNotAllowed means the actor may not change this account's limit.
	ErrNotAllowed = errors.New("credit: actor may not change this account's credit limit")
	// ErrNoAccount means the account does not exist.
	ErrNoAccount = errors.New("credit: account not found")
)

// Change is one credit limit change request.
type Change struct {
	AccountID      string
	NewLimit       decimal.Decimal
	ActorAccountID string // account of the user making the change
	ActorUser      string // user id or email, stored in the audit log
	Reason         string
}

// Service sets credit limits.
type Service struct {
	pool    *pgxpool.Pool
	wallets *wallet.Store
}

// New returns a Service. wallets may be nil when no Redis is in use (tests, tools).
func New(pool *pgxpool.Pool, wallets *wallet.Store) *Service {
	return &Service{pool: pool, wallets: wallets}
}

// SetLimit applies c. Platform accounts may change any limit; a reseller may
// change only its direct sub-accounts' limits; customers may change none.
func (s *Service) SetLimit(ctx context.Context, c Change) error {
	if c.NewLimit.IsNegative() {
		return wallet.ErrNegativeLimit
	}
	if strings.TrimSpace(c.Reason) == "" || strings.TrimSpace(c.ActorUser) == "" {
		return fmt.Errorf("credit: actor and reason are required")
	}

	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var parentID *string
		var actorRole string
		err := tx.QueryRow(ctx, `
			SELECT a.parent_id::text, actor.role
			FROM accounts a, accounts actor
			WHERE a.id = $1 AND actor.id = $2`, c.AccountID, c.ActorAccountID).Scan(&parentID, &actorRole)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNoAccount
		}
		if err != nil {
			return err
		}
		switch {
		case actorRole == "platform":
		case actorRole == "reseller" && parentID != nil && *parentID == c.ActorAccountID:
		default:
			return ErrNotAllowed
		}

		if _, err := tx.Exec(ctx, `SELECT set_config('app.actor', $1, true), set_config('app.reason', $2, true)`,
			c.ActorUser, c.Reason); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE wallets SET credit_limit = $2 WHERE account_id = $1`,
			c.AccountID, c.NewLimit)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23514" && strings.Contains(pgErr.Message, "reseller") {
			return fmt.Errorf("%w: %s", ErrResellerLimit, pgErr.Message)
		}
		return err
	})
	if err != nil {
		return err
	}

	if s.wallets != nil {
		// Committed; push only the limit. The Redis balance runs ahead of the
		// database between settlement batches, so it must not be overwritten here.
		if err := s.wallets.SetCreditLimit(ctx, c.AccountID, wallet.FromDecimal(c.NewLimit)); err != nil {
			return fmt.Errorf("credit: limit saved but cache update failed: %w", err)
		}
	}
	return nil
}
