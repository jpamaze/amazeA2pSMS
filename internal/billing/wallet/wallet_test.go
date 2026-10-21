package wallet

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/shopspring/decimal"
)

// newStore connects to REDIS_TEST_ADDR (default localhost:6379) and skips the
// test when no Redis is reachable.
func newStore(t *testing.T) (*Store, string) {
	t.Helper()
	addr := os.Getenv("REDIS_TEST_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skipf("redis not reachable at %s: %v", addr, err)
	}
	acct := fmt.Sprintf("test-%s-%d", t.Name(), time.Now().UnixNano())
	t.Cleanup(func() {
		rdb.Del(context.Background(), walletKey(acct), holdsKey(acct))
		rdb.Close()
	})
	return New(rdb), acct
}

func rupees(s string) Money { return FromDecimal(decimal.RequireFromString(s)) }

func TestDefaultCreditLimitIsPrepaidOnly(t *testing.T) {
	s, acct := newStore(t)
	ctx := context.Background()

	if err := s.Hold(ctx, acct, "h1", rupees("0.10")); !errors.Is(err, ErrInsufficient) {
		t.Fatalf("empty wallet with default credit limit 0: want ErrInsufficient, got %v", err)
	}
	if _, err := s.TopUp(ctx, acct, rupees("1.00")); err != nil {
		t.Fatal(err)
	}
	for i := range 10 {
		if err := s.Hold(ctx, acct, fmt.Sprintf("h%d", i), rupees("0.10")); err != nil {
			t.Fatalf("hold %d within balance: %v", i, err)
		}
	}
	if err := s.Hold(ctx, acct, "h-over", rupees("0.01")); !errors.Is(err, ErrInsufficient) {
		t.Fatalf("beyond balance with no credit: want ErrInsufficient, got %v", err)
	}
}

func TestCreditLimitExtendsAvailable(t *testing.T) {
	s, acct := newStore(t)
	ctx := context.Background()

	if _, err := s.TopUp(ctx, acct, rupees("1.00")); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCreditLimit(ctx, acct, rupees("0.50")); err != nil {
		t.Fatal(err)
	}
	if err := s.Hold(ctx, acct, "a", rupees("1.50")); err != nil {
		t.Fatalf("hold up to wallet + credit: %v", err)
	}
	if err := s.Hold(ctx, acct, "b", rupees("0.0001")); !errors.Is(err, ErrInsufficient) {
		t.Fatalf("one unit over wallet + credit: want ErrInsufficient, got %v", err)
	}
	bal, err := s.Settle(ctx, acct, "a", rupees("1.50"))
	if err != nil {
		t.Fatal(err)
	}
	if bal != rupees("-0.50") {
		t.Fatalf("balance after using credit: want -0.50, got %s", bal.Decimal())
	}
	snap, _ := s.Get(ctx, acct)
	if snap.CreditUsed() != rupees("0.50") || snap.Available() != 0 {
		t.Fatalf("snapshot: credit used %s, available %s", snap.CreditUsed().Decimal(), snap.Available().Decimal())
	}

	// A top-up repays the credit used first.
	if _, err := s.TopUp(ctx, acct, rupees("2.00")); err != nil {
		t.Fatal(err)
	}
	snap, _ = s.Get(ctx, acct)
	if snap.Balance != rupees("1.50") || snap.CreditUsed() != 0 {
		t.Fatalf("after top-up: balance %s, credit used %s", snap.Balance.Decimal(), snap.CreditUsed().Decimal())
	}
}

func TestLoweringLimitBelowUsedBlocksNewHolds(t *testing.T) {
	s, acct := newStore(t)
	ctx := context.Background()

	_ = s.SetCreditLimit(ctx, acct, rupees("1.00"))
	if err := s.Hold(ctx, acct, "a", rupees("0.80")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Settle(ctx, acct, "a", rupees("0.80")); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCreditLimit(ctx, acct, rupees("0.50")); err != nil {
		t.Fatalf("lowering below credit used must be allowed: %v", err)
	}
	if err := s.Hold(ctx, acct, "b", rupees("0.01")); !errors.Is(err, ErrInsufficient) {
		t.Fatalf("want ErrInsufficient after lowering limit, got %v", err)
	}
	if err := s.SetCreditLimit(ctx, acct, -1); !errors.Is(err, ErrNegativeLimit) {
		t.Fatalf("negative limit: want ErrNegativeLimit, got %v", err)
	}
}

func TestHoldIsIdempotentAndReleaseRestores(t *testing.T) {
	s, acct := newStore(t)
	ctx := context.Background()

	_, _ = s.TopUp(ctx, acct, rupees("1.00"))
	for range 3 {
		if err := s.Hold(ctx, acct, "same", rupees("0.60")); err != nil {
			t.Fatal(err)
		}
	}
	snap, _ := s.Get(ctx, acct)
	if snap.Held != rupees("0.60") {
		t.Fatalf("retried hold must count once: held %s", snap.Held.Decimal())
	}
	if err := s.Release(ctx, acct, "same"); err != nil {
		t.Fatal(err)
	}
	if err := s.Release(ctx, acct, "same"); !errors.Is(err, ErrUnknownHold) {
		t.Fatalf("second release: want ErrUnknownHold, got %v", err)
	}
	if _, err := s.Settle(ctx, acct, "never", 1); !errors.Is(err, ErrUnknownHold) {
		t.Fatalf("settle unknown: want ErrUnknownHold, got %v", err)
	}
	snap, _ = s.Get(ctx, acct)
	if snap.Held != 0 || snap.Available() != rupees("1.00") {
		t.Fatalf("after release: held %s available %s", snap.Held.Decimal(), snap.Available().Decimal())
	}
}

func TestConcurrentHoldsNeverOverspend(t *testing.T) {
	s, acct := newStore(t)
	ctx := context.Background()

	// ₹10 wallet + ₹5 credit = 1,500 messages at ₹0.01.
	_, _ = s.TopUp(ctx, acct, rupees("10"))
	_ = s.SetCreditLimit(ctx, acct, rupees("5"))

	var ok, denied atomic.Int64
	var wg sync.WaitGroup
	for w := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 40 {
				err := s.Hold(ctx, acct, fmt.Sprintf("w%d-%d", w, i), rupees("0.01"))
				switch {
				case err == nil:
					ok.Add(1)
				case errors.Is(err, ErrInsufficient):
					denied.Add(1)
				default:
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()

	if ok.Load() != 1500 || denied.Load() != 500 {
		t.Fatalf("want 1500 held and 500 denied, got %d and %d", ok.Load(), denied.Load())
	}
	snap, _ := s.Get(ctx, acct)
	if snap.Available() != 0 {
		t.Fatalf("available after exhausting: %s", snap.Available().Decimal())
	}
}

func TestMoneyConversion(t *testing.T) {
	if got := rupees("0.1234"); got != 1234 {
		t.Fatalf("0.1234 -> %d", got)
	}
	if got := Money(-5000).Decimal().String(); got != "-0.5" {
		t.Fatalf("-5000 -> %s", got)
	}
}

func TestTopUpAndLimitLoadMissingWallet(t *testing.T) {
	s, acct := newStore(t)
	ctx := context.Background()

	// Wallet not in Redis: the top-up loads the database values (which
	// already include it) instead of adding to zero.
	bal, err := s.ApplyTopUp(ctx, acct, rupees("50"), rupees("150"), rupees("10"))
	if err != nil || bal != rupees("150") {
		t.Fatalf("load on top-up: %s %v", bal.Decimal(), err)
	}
	snap, _ := s.Get(ctx, acct)
	if snap.CreditLimit != rupees("10") || snap.Available() != rupees("160") {
		t.Fatalf("loaded wallet: %+v", snap)
	}

	// Loaded: only the delta is applied (the Redis balance may be ahead of the DB).
	_ = s.Hold(ctx, acct, "h", rupees("20"))
	_, _ = s.Settle(ctx, acct, "h", rupees("20"))
	bal, _ = s.ApplyTopUp(ctx, acct, rupees("5"), rupees("999"), rupees("10"))
	if bal != rupees("135") {
		t.Fatalf("delta top-up: %s", bal.Decimal())
	}

	// Credit limit on a loaded wallet changes only the limit.
	if err := s.ApplyCreditLimit(ctx, acct, rupees("25"), rupees("999")); err != nil {
		t.Fatal(err)
	}
	snap, _ = s.Get(ctx, acct)
	if snap.Balance != rupees("135") || snap.CreditLimit != rupees("25") {
		t.Fatalf("limit change touched balance: %+v", snap)
	}
}
