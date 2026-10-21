package firewall

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/redis/go-redis/v9"

	"github.com/jpamaze/amazeA2pSMS/internal/platform/pgtest"
)

func TestRules(t *testing.T) {
	rules := []*Rule{
		{ID: 1, Kind: KindSender, Pattern: "*BANK", Action: "reject"},
		{ID: 2, Kind: KindContent, Pattern: "free recharge", Action: "reject"},
		{ID: 3, Kind: KindContent, Pattern: `re:\bkyc\b.*\bblock`, Action: "reject"},
		{ID: 4, Kind: KindURL, Pattern: "https://evil.in/", Action: "reject"},
		{ID: 5, Kind: KindDestination, Pattern: "9198100*", Action: "reject"},
		{ID: 6, Kind: KindContent, Pattern: "lottery", Action: "flag"},
		{ID: 7, AccountID: "acct-b", Kind: KindSender, Pattern: "AMZTST", Action: "reject"},
	}
	for _, r := range rules {
		if err := r.Compile(); err != nil {
			t.Fatalf("rule %d: %v", r.ID, err)
		}
	}
	f := &Firewall{}
	f.rules.Store(&rules)
	cases := []struct {
		acct, sender, dest, text string
		want                     Reason
	}{
		{"a", "AMZTST", "919820000001", "Your OTP is 1234", ""},
		{"a", "fakebank", "919820000001", "hi", ReasonSender},
		{"a", "AMZTST", "919820000001", "Get FREE  Recharge now", ReasonContent},
		{"a", "AMZTST", "919820000001", "freerecharge", ""}, // whole words only
		{"a", "AMZTST", "919820000001", "Your KYC is pending, account will be BLOCKED", ReasonContent},
		{"a", "AMZTST", "919820000001", "pay at https://pay.evil.in/x", ReasonURL},
		{"a", "AMZTST", "919820000001", "see notevil.in/x", ""},
		{"a", "AMZTST", "919810012345", "hi", ReasonDestination},
		{"a", "AMZTST", "919820000001", "you won the lottery", ""}, // flag only
		{"acct-b", "AMZTST", "919820000001", "hi", ReasonSender},   // account rule
	}
	for _, c := range cases {
		e := f.CheckRules(c.acct, c.sender, c.dest, c.text)
		if (e == nil) != (c.want == "") || (e != nil && e.Reason != c.want) {
			t.Errorf("%+v: got %v", c, e)
		}
	}
	for _, bad := range []Rule{{Kind: "nope", Pattern: "x", Action: "reject"}, {Kind: KindSender, Pattern: "x", Action: "drop"},
		{Kind: KindContent, Pattern: "re:(", Action: "reject"}, {Kind: KindSender, Pattern: " ", Action: "reject"}} {
		if err := bad.Compile(); err == nil {
			t.Errorf("%+v must not compile", bad)
		}
	}
}

func TestVelocity(t *testing.T) {
	addr := os.Getenv("REDIS_TEST_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}
	r := redis.NewClient(&redis.Options{Addr: addr})
	ctx := context.Background()
	if err := r.Ping(ctx).Err(); err != nil {
		t.Skipf("redis not reachable: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	pool := pgtest.Pool(t)
	var acct string
	if err := pool.QueryRow(ctx, `INSERT INTO accounts (role, name) VALUES ('platform', 'P') RETURNING id`).Scan(&acct); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		iter := r.Scan(ctx, 0, "fw:{"+acct+"}:*", 1000).Iterator()
		for iter.Next(ctx) {
			r.Del(ctx, iter.Val())
		}
	})
	if _, err := pool.Exec(ctx, `INSERT INTO firewall_limits (account_id, dest_per_hour, series_per_10min, series_block_minutes, updated_by)
		VALUES ($1, 3, 10, 5, 't')`, acct); err != nil {
		t.Fatal(err)
	}
	f := &Firewall{PG: pool, Redis: r}
	// A Redis restart drops loaded scripts: the pipelined EVALSHA must recover.
	if err := r.ScriptFlush(ctx).Err(); err != nil {
		t.Fatal(err)
	}

	// One number: 3 an hour.
	for i := range 4 {
		err := f.Check(ctx, acct, "AMZTST", "919820000001", "hi")
		if (i < 3) != (err == nil) {
			t.Fatalf("message %d to one number: %v", i+1, err)
		}
		if i == 3 {
			if e, _ := AsError(err); e.Reason != ReasonDestRate {
				t.Fatalf("reason: %v", err)
			}
		}
	}
	// One 1,000-number range: 10 per 10 minutes, then the range is shut.
	// (Message 4 to ...0001 above was refused before it was counted in the range.)
	var err error
	sent := 3
	for i := 2; sent < 10; i++ {
		if err = f.Check(ctx, acct, "AMZTST", fmt.Sprintf("919820000%03d", i), "hi"); err != nil {
			t.Fatalf("range message %d: %v", sent+1, err)
		}
		sent++
	}
	err = f.Check(ctx, acct, "AMZTST", "919820000500", "hi")
	if e, ok := AsError(err); !ok || e.Reason != ReasonSeries {
		t.Fatalf("11th into the range: %v", err)
	}
	if err := f.Check(ctx, acct, "AMZTST", "919820000999", "hi"); err == nil {
		t.Fatal("range stays shut")
	}
	if err := f.Check(ctx, acct, "AMZTST", "919820001000", "hi"); err != nil {
		t.Fatalf("next range is open: %v", err)
	}
	b, _ := Blocked(ctx, r, acct)
	if ttl, ok := b["919820000xxx"]; !ok || ttl <= 0 || ttl > 300 {
		t.Fatalf("blocked: %v", b)
	}
	if ok, _ := Unblock(ctx, r, acct, "919820000xxx"); !ok {
		t.Fatal("unblock")
	}
	if err := f.Check(ctx, acct, "AMZTST", "919820000777", "hi"); err != nil {
		t.Fatalf("unblocked range takes traffic again: %v", err)
	}
}
