package deals

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/jpamaze/amazeA2pSMS/internal/platform/pgtest"
)

func redisClient(t *testing.T) redis.UniversalClient {
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
	t.Cleanup(func() { rdb.Close() })
	return rdb
}

func TestFindAndReserve(t *testing.T) {
	rdb := redisClient(t)
	pool := pgtest.Pool(t)
	ctx := context.Background()
	var acct string
	if err := pool.QueryRow(ctx, `INSERT INTO accounts (role, name) VALUES ('platform', 'P') RETURNING id`).Scan(&acct); err != nil {
		t.Fatal(err)
	}
	add := func(name, mnc, price string, capSegs any, starts, ends time.Duration, status string) string {
		var id string
		if err := pool.QueryRow(ctx, `INSERT INTO customer_deals (account_id, name, mcc, mnc, price, cap_segments, starts_at, ends_at, status, created_by)
			VALUES ($1, $2, '404', $3, $4, $5, now() + $6::interval, now() + $7::interval, $8, 't') RETURNING id`,
			acct, name, mnc, price, capSegs, starts.String(), ends.String(), status).Scan(&id); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { rdb.Del(context.Background(), usedKey(id), reservedKey(id)) })
		return id
	}
	country := add("country", "*", "0.10", 100, -time.Hour, time.Hour, "active")
	network := add("network", "45", "0.08", nil, -time.Hour, time.Hour, "active")
	add("ended", "*", "0.01", nil, -2*time.Hour, -time.Hour, "active")
	add("later", "*", "0.01", nil, time.Hour, 2*time.Hour, "active")
	add("cancelled", "10", "0.01", nil, -time.Hour, time.Hour, "cancelled")

	b := &Book{PG: pool, Redis: rdb}
	if d, err := b.Find(ctx, acct, "404", "45"); err != nil || d == nil || d.ID != network {
		t.Fatalf("network deal must win: %+v %v", d, err)
	}
	if d, _ := b.Find(ctx, acct, "404", "10"); d == nil || d.ID != country || d.Cap != 100 {
		t.Fatalf("country deal (cancelled network deal ignored): %+v", d)
	}
	if d, _ := b.Find(ctx, acct, "405", "10"); d != nil {
		t.Fatalf("other country: %+v", d)
	}

	// 30 concurrent 5-segment messages against a cap of 100: exactly 20 fit.
	d, _ := b.Find(ctx, acct, "404", "10")
	var ok atomic.Int32
	var wg sync.WaitGroup
	for i := range 30 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, _, err := Reserve(ctx, rdb, d, "m"+string(rune('A'+i)), 5)
			if err != nil {
				t.Error(err)
			}
			if got {
				ok.Add(1)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 20 {
		t.Fatalf("reserved %d messages, want 20", ok.Load())
	}
	used := func() int64 { n, _ := Used(ctx, rdb, []string{d.ID}); return n[d.ID] }
	if used() != 100 {
		t.Fatalf("used %d", used())
	}

	// Find a message that got in; releasing it twice gives 5 back once.
	var in string
	for i := range 30 {
		m := "m" + string(rune('A'+i))
		if rdb.HExists(ctx, reservedKey(d.ID), m).Val() {
			in = m
			break
		}
	}
	if again, _, _ := Reserve(ctx, rdb, d, in, 5); !again || used() != 100 {
		t.Fatal("re-reserving the same message must be a no-op")
	}
	_ = Release(ctx, rdb, d.ID, in)
	_ = Release(ctx, rdb, d.ID, in)
	if used() != 95 {
		t.Fatalf("after release: %d", used())
	}
	// Settled volume stays used and cannot be released later.
	var other string
	for i := range 30 {
		m := "m" + string(rune('A'+i))
		if m != in && rdb.HExists(ctx, reservedKey(d.ID), m).Val() {
			other = m
			break
		}
	}
	if err := Apply(ctx, rdb, []Op{{DealID: d.ID, MsgID: other}}); err != nil {
		t.Fatal(err)
	}
	if err := Apply(ctx, rdb, []Op{{DealID: d.ID, MsgID: other, Release: true}, {DealID: d.ID, MsgID: in, Release: true}}); err != nil {
		t.Fatal(err)
	}
	if used() != 95 {
		t.Fatalf("settled volume must stay used: %d", used())
	}
}

func TestReserveSeedsFromDatabase(t *testing.T) {
	rdb := redisClient(t)
	d := &Deal{ID: "seed-" + time.Now().Format("150405.000000"), Cap: 10, Used: 8, Ends: time.Now().Add(time.Hour)}
	t.Cleanup(func() { rdb.Del(context.Background(), usedKey(d.ID), reservedKey(d.ID)) })
	ctx := context.Background()
	if ok, left, _ := Reserve(ctx, rdb, d, "a", 3); ok || left != 2 {
		t.Fatalf("8 charged + 3 exceeds the cap of 10 (left %d)", left)
	}
	if ok, left, _ := Reserve(ctx, rdb, d, "b", 2); !ok || left != 0 {
		t.Fatalf("8 charged + 2 fits exactly (left %d)", left)
	}
	// The book remembers a used-up deal and stops asking Redis.
	b := &Book{Redis: rdb}
	if ok, _ := b.Reserve(ctx, d, "c", 1); ok {
		t.Fatal("cap reached")
	}
	_ = Release(ctx, rdb, d.ID, "b")
	if ok, _ := b.Reserve(ctx, d, "c", 1); ok {
		t.Fatal("a used-up deal is not retried before the cache refreshes")
	}
	if ok, _, _ := Reserve(ctx, rdb, d, "c", 1); !ok {
		t.Fatal("released volume is reservable")
	}
	if ttl := rdb.PTTL(ctx, usedKey(d.ID)).Val(); ttl < 7*24*time.Hour {
		t.Fatalf("counter must outlive the deal by a week: %v", ttl)
	}
}
