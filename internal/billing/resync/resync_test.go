package resync

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/shopspring/decimal"

	"github.com/jpamaze/amazeA2pSMS/internal/billing/ledger"
	"github.com/jpamaze/amazeA2pSMS/internal/billing/wallet"
	"github.com/jpamaze/amazeA2pSMS/internal/platform/pgtest"
)

// v7 makes a message ID accepted at t.
func v7(t time.Time) string {
	var u uuid.UUID
	_, _ = rand.Read(u[:])
	var ms [8]byte
	binary.BigEndian.PutUint64(ms[:], uint64(t.UnixMilli()))
	copy(u[:6], ms[2:])
	u[6] = u[6]&0x0f | 0x70
	u[8] = u[8]&0x3f | 0x80
	return u.String()
}

func TestResyncRepairsDriftAndStaleHolds(t *testing.T) {
	addr := os.Getenv("REDIS_TEST_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}
	ctx := context.Background()
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skipf("redis not reachable: %v", err)
	}
	t.Cleanup(func() { rdb.Close() })
	pool := pgtest.Pool(t)
	ws := wallet.New(rdb)
	led := &ledger.Service{PG: pool, Wallets: ws}

	var plat, acct string
	if err := pool.QueryRow(ctx, `INSERT INTO accounts (role, name) VALUES ('platform', 'P') RETURNING id`).Scan(&plat); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO accounts (parent_id, role, name) VALUES ($1, 'customer', 'C') RETURNING id`, plat).Scan(&acct); err != nil {
		t.Fatal(err)
	}
	if _, err := led.TopUp(ctx, ledger.TopUp{AccountID: acct, Amount: decimal.NewFromInt(100), IdempotencyKey: "k" + acct, Actor: "t"}); err != nil {
		t.Fatal(err)
	}

	// What a lost write after a failover leaves behind: a settle that never
	// reached the new primary (balance too high), held out of step, holds of
	// messages long since sent, and holds that must stay.
	now := time.Now()
	sent, waiting, fresh, gone := v7(now.Add(-2*time.Hour)), v7(now.Add(-2*time.Hour)), v7(now.Add(-time.Minute)), v7(now.Add(-3*time.Hour))
	for id, st := range map[string]string{sent: "delivered", waiting: "accepted"} {
		rdb.HSet(ctx, "msg:{"+id+"}", "status", st)
		t.Cleanup(func() { rdb.Del(context.Background(), "msg:{"+id+"}") })
	}
	for _, id := range []string{sent, waiting, fresh, gone} {
		if err := ws.Hold(ctx, acct, id, 1000); err != nil {
			t.Fatal(err)
		}
	}
	rdb.HSet(ctx, "wallet:{"+acct+"}", "balance", 1_300_000, "held", 999)

	rep, err := Run(ctx, pool, rdb, Options{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Drifts) != 1 || rep.Drifts[0].StaleHolds != 2 || rep.Drifts[0].RedisBalance != 1_300_000 || rep.Drifts[0].Balance != 1_000_000 {
		t.Fatalf("dry run: %+v", rep)
	}
	if s, _ := ws.Get(ctx, acct); s.Balance != 1_300_000 {
		t.Fatalf("dry run changed Redis: %+v", s)
	}

	if _, err := Run(ctx, pool, rdb, Options{}); err != nil {
		t.Fatal(err)
	}
	s, _ := ws.Get(ctx, acct)
	holds, _ := ws.Holds(ctx, acct)
	if s.Balance != 1_000_000 || s.Held != 2000 || len(holds) != 2 || holds[waiting] != 1000 || holds[fresh] != 1000 {
		t.Fatalf("after repair: %+v holds %v", s, holds)
	}
	if rep, _ := Run(ctx, pool, rdb, Options{DryRun: true}); len(rep.Drifts) != 0 {
		t.Fatalf("second run still differs: %+v", rep.Drifts)
	}

	// A resync waits for balance changes in progress (shared lock holders).
	release := make(chan struct{})
	locked := make(chan struct{})
	go func() {
		_ = wallet.Shared(ctx, pool, func(*pgxpool.Conn) error { close(locked); <-release; return nil })
	}()
	<-locked
	done := make(chan error, 1)
	go func() { _, err := Run(ctx, pool, rdb, Options{}); done <- err }()
	select {
	case <-done:
		t.Fatal("resync ran while a balance change held the lock")
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
