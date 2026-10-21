package ledger

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/shopspring/decimal"

	"github.com/jpamaze/amazeA2pSMS/internal/billing/wallet"
	"github.com/jpamaze/amazeA2pSMS/internal/platform/pgtest"
)

func setup(t *testing.T) (*Service, *pgxpool.Pool) {
	t.Helper()
	addr := os.Getenv("REDIS_TEST_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skipf("redis not reachable: %v", err)
	}
	t.Cleanup(func() { rdb.Close() })
	pool := pgtest.Pool(t)
	return &Service{PG: pool, Wallets: wallet.New(rdb)}, pool
}

func TestConcurrentTransfersNeverOverspend(t *testing.T) {
	s, pool := setup(t)
	ctx := context.Background()
	var platform, reseller, shop string
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(pool.QueryRow(ctx, `INSERT INTO accounts (role, name) VALUES ('platform', 'P') RETURNING id`).Scan(&platform))
	must(pool.QueryRow(ctx, `INSERT INTO accounts (parent_id, role, name) VALUES ($1, 'reseller', 'R') RETURNING id`, platform).Scan(&reseller))
	must(pool.QueryRow(ctx, `INSERT INTO accounts (parent_id, role, name) VALUES ($1, 'customer', 'S') RETURNING id`, reseller).Scan(&shop))
	_, err := s.TopUp(ctx, TopUp{AccountID: reseller, Amount: decimal.NewFromInt(100), IdempotencyKey: "t" + reseller, Actor: "test"})
	must(err)

	// 20 concurrent transfers of Rs 10 against Rs 100: exactly 10 succeed.
	var ok, refused atomic.Int32
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.Transfer(ctx, Transfer{From: reseller, To: shop, Amount: decimal.NewFromInt(10),
				IdempotencyKey: fmt.Sprintf("x-%d", i), Actor: "test"})
			switch {
			case err == nil:
				ok.Add(1)
			case errors.Is(err, wallet.ErrInsufficient):
				refused.Add(1)
			default:
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 10 || refused.Load() != 10 {
		t.Fatalf("want 10 ok / 10 refused, got %d / %d", ok.Load(), refused.Load())
	}

	var rb, sb decimal.Decimal
	must(pool.QueryRow(ctx, `SELECT balance FROM wallets WHERE account_id = $1`, reseller).Scan(&rb))
	must(pool.QueryRow(ctx, `SELECT balance FROM wallets WHERE account_id = $1`, shop).Scan(&sb))
	snapR, _ := s.Wallets.Get(ctx, reseller)
	snapS, _ := s.Wallets.Get(ctx, shop)
	if !rb.IsZero() || !sb.Equal(decimal.NewFromInt(100)) ||
		snapR.Balance != 0 || snapS.Balance != wallet.FromDecimal(decimal.NewFromInt(100)) {
		t.Fatalf("balances: db %s/%s redis %s/%s", rb, sb, snapR.Balance.Decimal(), snapS.Balance.Decimal())
	}

	// A repeated key is refused and the debit is refunded.
	must(pool.QueryRow(ctx, `UPDATE wallets SET balance = balance + 5 WHERE account_id = $1 RETURNING balance`, reseller).Scan(&rb))
	_ = s.Wallets.Credit(ctx, reseller, wallet.FromDecimal(decimal.NewFromInt(5)))
	var booked string
	must(pool.QueryRow(ctx, `SELECT split_part(idempotency_key, ':', 2) FROM ledger_entries WHERE kind = 'transfer_out' LIMIT 1`).Scan(&booked))
	if _, err := s.Transfer(ctx, Transfer{From: reseller, To: shop, Amount: decimal.NewFromInt(1), IdempotencyKey: booked, Actor: "t"}); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate: %v", err)
	}
	if snap, _ := s.Wallets.Get(ctx, reseller); snap.Balance != wallet.FromDecimal(decimal.NewFromInt(5)) {
		t.Fatalf("debit not refunded after duplicate: %s", snap.Balance.Decimal())
	}

	// Ledger: both sides recorded with the counterparty.
	var n int
	must(pool.QueryRow(ctx, `SELECT count(*) FROM ledger_entries WHERE kind IN ('transfer_in','transfer_out') AND counterparty IS NOT NULL`).Scan(&n))
	if n != 20 {
		t.Fatalf("ledger transfer rows: %d", n)
	}
}
