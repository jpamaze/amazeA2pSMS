// Package wallet places, settles and releases balance holds in Redis.
//
// An account may send while
//
//	available = balance + credit_limit - held >= cost
//
// Balance may go negative, down to -credit_limit. The credit limit defaults to
// 0, so a new account is prepaid only. Every operation is one Lua script, so
// concurrent holds never overspend and no database row is locked per message
// (blueprint rule 4). PostgreSQL stays the system of record: the rater writes
// ledger entries in batches and Sync reloads Redis from it.
package wallet

import (
	"context"
	_ "embed"
	"errors"
	"fmt"

	"github.com/redis/go-redis/v9"
	"github.com/shopspring/decimal"
)

// Units per currency unit: amounts are int64 with 4 decimal places, so
// ₹0.1234 per SMS is exactly 1234 units.
const Units = 10_000

// Money is an amount in 1/10,000 of a currency unit.
type Money int64

// FromDecimal converts a decimal amount, rounding to 4 places.
func FromDecimal(d decimal.Decimal) Money {
	return Money(d.Mul(decimal.NewFromInt(Units)).Round(0).IntPart())
}

// Decimal converts back to a decimal amount.
func (m Money) Decimal() decimal.Decimal {
	return decimal.New(int64(m), -4)
}

var (
	// ErrInsufficient means balance + credit limit - holds does not cover the amount.
	ErrInsufficient = errors.New("wallet: insufficient balance")
	// ErrUnknownHold means the hold was never placed or is already settled/released.
	ErrUnknownHold = errors.New("wallet: unknown hold")
	// ErrNegativeLimit rejects a credit limit below zero.
	ErrNegativeLimit = errors.New("wallet: credit limit must be >= 0")
)

var (
	//go:embed hold.lua
	holdSrc string
	//go:embed settle.lua
	settleSrc string
	//go:embed release.lua
	releaseSrc string

	holdScript    = redis.NewScript(holdSrc)
	settleScript  = redis.NewScript(settleSrc)
	releaseScript = redis.NewScript(releaseSrc)
)

// Store is the Redis-backed wallet.
type Store struct {
	rdb redis.UniversalClient
}

// New returns a Store using rdb.
func New(rdb redis.UniversalClient) *Store { return &Store{rdb: rdb} }

// Keys share the {accountID} hash tag so both land on one cluster slot.
func walletKey(accountID string) string { return fmt.Sprintf("wallet:{%s}", accountID) }
func holdsKey(accountID string) string  { return fmt.Sprintf("wallet:{%s}:holds", accountID) }

// Snapshot is the current state of a wallet.
type Snapshot struct {
	Balance     Money
	CreditLimit Money
	Held        Money
}

// Available is what the account can still spend.
func (s Snapshot) Available() Money { return s.Balance + s.CreditLimit - s.Held }

// CreditUsed is how much of the credit limit the balance has consumed.
func (s Snapshot) CreditUsed() Money {
	if s.Balance >= 0 {
		return 0
	}
	return -s.Balance
}

// Hold reserves amount under holdID. Placing the same holdID twice is a no-op,
// so a retried submit never double-holds.
func (s *Store) Hold(ctx context.Context, accountID, holdID string, amount Money) error {
	if amount <= 0 {
		return fmt.Errorf("wallet: hold amount must be > 0, got %d", amount)
	}
	res, err := holdScript.Run(ctx, s.rdb,
		[]string{walletKey(accountID), holdsKey(accountID)}, holdID, int64(amount)).Int()
	if err != nil {
		return fmt.Errorf("wallet: hold: %w", err)
	}
	if res == 0 {
		return ErrInsufficient
	}
	return nil
}

// Settle closes a hold and charges actual (which may differ from the held
// amount, for example after segment recount). It returns the new balance.
func (s *Store) Settle(ctx context.Context, accountID, holdID string, actual Money) (Money, error) {
	res, err := settleScript.Run(ctx, s.rdb,
		[]string{walletKey(accountID), holdsKey(accountID)}, holdID, int64(actual)).Slice()
	if err != nil {
		return 0, fmt.Errorf("wallet: settle: %w", err)
	}
	if res[0].(int64) == 0 {
		return 0, ErrUnknownHold
	}
	return Money(res[1].(int64)), nil
}

// Release closes a hold without charging (message rejected or failed before send).
func (s *Store) Release(ctx context.Context, accountID, holdID string) error {
	res, err := releaseScript.Run(ctx, s.rdb,
		[]string{walletKey(accountID), holdsKey(accountID)}, holdID).Int()
	if err != nil {
		return fmt.Errorf("wallet: release: %w", err)
	}
	if res == 0 {
		return ErrUnknownHold
	}
	return nil
}

// TopUp adds to the balance. A negative balance (credit used) is repaid first
// simply because it is added to.
func (s *Store) TopUp(ctx context.Context, accountID string, amount Money) (Money, error) {
	if amount <= 0 {
		return 0, fmt.Errorf("wallet: top-up must be > 0, got %d", amount)
	}
	v, err := s.rdb.HIncrBy(ctx, walletKey(accountID), "balance", int64(amount)).Result()
	return Money(v), err
}

//go:embed setlimit.lua
var setLimitSrc string
var setLimitScript = redis.NewScript(setLimitSrc)

// ApplyCreditLimit sets a committed credit limit on the hot copy, loading the
// wallet from dbBalance if it is not in Redis yet.
func (s *Store) ApplyCreditLimit(ctx context.Context, accountID string, limit, dbBalance Money) error {
	if limit < 0 {
		return ErrNegativeLimit
	}
	return setLimitScript.Run(ctx, s.rdb, []string{walletKey(accountID)}, int64(limit), int64(dbBalance)).Err()
}

//go:embed debit.lua
var debitSrc string
var debitScript = redis.NewScript(debitSrc)

// Debit takes amount from a wallet if its available balance covers it
// (loading the wallet from the database values when Redis lacks it).
func (s *Store) Debit(ctx context.Context, accountID string, amount, dbBalance, dbCreditLimit Money) (Money, error) {
	v, err := debitScript.Run(ctx, s.rdb, []string{walletKey(accountID)},
		int64(amount), int64(dbBalance), int64(dbCreditLimit)).Int64()
	if errors.Is(err, redis.Nil) {
		return 0, ErrInsufficient
	}
	return Money(v), err
}

// Credit adds amount to a wallet already in Redis (used to undo a Debit).
func (s *Store) Credit(ctx context.Context, accountID string, amount Money) error {
	return s.rdb.HIncrBy(ctx, walletKey(accountID), "balance", int64(amount)).Err()
}

//go:embed topup.lua
var topupSrc string
var topupScript = redis.NewScript(topupSrc)

// ApplyTopUp adds a committed top-up to the hot copy. If the wallet is not in
// Redis yet (new account, or Redis was rebuilt) it is loaded from the
// database values, which already include the top-up.
func (s *Store) ApplyTopUp(ctx context.Context, accountID string, amount, dbBalance, dbCreditLimit Money) (Money, error) {
	v, err := topupScript.Run(ctx, s.rdb, []string{walletKey(accountID)},
		int64(amount), int64(dbBalance), int64(dbCreditLimit)).Int64()
	return Money(v), err
}

// SetCreditLimit changes the credit limit. Lowering it below credit already
// used is allowed: new holds fail until a top-up brings available above zero.
// Reseller and audit rules are enforced by the caller (admin-api) and the
// database; this only updates the hot copy.
func (s *Store) SetCreditLimit(ctx context.Context, accountID string, limit Money) error {
	if limit < 0 {
		return ErrNegativeLimit
	}
	return s.rdb.HSet(ctx, walletKey(accountID), "credit_limit", int64(limit)).Err()
}

// Sync loads balance and credit limit from the system of record, leaving
// open holds untouched.
func (s *Store) Sync(ctx context.Context, accountID string, balance, creditLimit Money) error {
	if creditLimit < 0 {
		return ErrNegativeLimit
	}
	return s.rdb.HSet(ctx, walletKey(accountID),
		"balance", int64(balance), "credit_limit", int64(creditLimit)).Err()
}

// Get returns the wallet's current state. A wallet never written reads as
// zero balance and zero credit limit.
func (s *Store) Get(ctx context.Context, accountID string) (Snapshot, error) {
	vals, err := s.rdb.HMGet(ctx, walletKey(accountID), "balance", "credit_limit", "held").Result()
	if err != nil {
		return Snapshot{}, err
	}
	var out [3]int64
	for i, v := range vals {
		if v == nil {
			continue
		}
		if _, err := fmt.Sscan(v.(string), &out[i]); err != nil {
			return Snapshot{}, fmt.Errorf("wallet: bad field: %w", err)
		}
	}
	return Snapshot{Balance: Money(out[0]), CreditLimit: Money(out[1]), Held: Money(out[2])}, nil
}

//go:embed repair.lua
var repairSrc string

var repairScript = redis.NewScript(repairSrc)

// Holds returns the open holds of an account (hold ID -> amount).
func (s *Store) Holds(ctx context.Context, accountID string) (map[string]Money, error) {
	v, err := s.rdb.HGetAll(ctx, holdsKey(accountID)).Result()
	if err != nil {
		return nil, err
	}
	out := make(map[string]Money, len(v))
	for id, a := range v {
		var n int64
		if _, err := fmt.Sscan(a, &n); err != nil {
			return nil, fmt.Errorf("wallet: bad hold %s: %w", id, err)
		}
		out[id] = Money(n)
	}
	return out, nil
}

// Repair sets balance and credit limit from the system of record, drops the
// given holds and recomputes held from the holds left, atomically. Callers
// hold the sync lock exclusively (see Exclusive). It returns held before and
// after, and how many holds were dropped.
func (s *Store) Repair(ctx context.Context, accountID string, balance, creditLimit Money, drop []string) (before, after Money, dropped int, err error) {
	if creditLimit < 0 {
		return 0, 0, 0, ErrNegativeLimit
	}
	args := make([]any, 0, 2+len(drop))
	args = append(args, int64(balance), int64(creditLimit))
	for _, d := range drop {
		args = append(args, d)
	}
	res, err := repairScript.Run(ctx, s.rdb, []string{walletKey(accountID), holdsKey(accountID)}, args...).Int64Slice()
	if err != nil {
		return 0, 0, 0, fmt.Errorf("wallet: repair: %w", err)
	}
	return Money(res[0]), Money(res[1]), int(res[2]), nil
}
