package vendorhealth

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

var pol = Policy{Enabled: true, MaxSubmitMs: 1000, MaxErrorPct: 50, MinDLRPct: 60, DLRWindow: 15 * time.Minute, Cooldown: 30 * time.Second}

func rep(up, binds, ok, failed, p95 int) Report {
	return Report{At: time.Now().UnixMilli(), BindsUp: up, Binds: binds, OK: ok, Failed: failed, P95Ms: p95}
}

func TestDecide(t *testing.T) {
	now := time.Now()
	up := State{Up: true, Since: now.Add(-time.Hour).UnixMilli()}
	cases := []struct {
		name string
		sig  Signals
		want string // "" = up
	}{
		{"healthy", Signals{Reports: []Report{rep(2, 2, 500, 3, 120)}, Finals: 1000, Delivered: 900}, ""},
		{"no connector", Signals{}, "down"},
		{"all binds down", Signals{Reports: []Report{rep(0, 2, 0, 0, 0), rep(0, 2, 0, 0, 0)}}, "down"},
		{"one instance has binds", Signals{Reports: []Report{rep(0, 2, 0, 0, 0), rep(1, 2, 100, 0, 50)}}, ""},
		{"HTTP vendor (no binds)", Signals{Reports: []Report{rep(1, 0, 100, 0, 50)}}, ""},
		{"errors", Signals{Reports: []Report{rep(2, 2, 40, 60, 100)}}, "errors"},
		{"errors, too few samples", Signals{Reports: []Report{rep(2, 2, 5, 10, 100)}}, ""},
		{"slow", Signals{Reports: []Report{rep(2, 2, 100, 0, 2500)}}, "slow"},
		{"low delivery rate", Signals{Reports: []Report{rep(2, 2, 100, 0, 100)}, Finals: 500, Delivered: 200}, "dlr_rate"},
		{"low delivery, too few receipts", Signals{Reports: []Report{rep(2, 2, 100, 0, 100)}, Finals: 100, Delivered: 10}, ""},
	}
	for _, c := range cases {
		got := Decide(pol, c.sig, up, now)
		if (c.want == "") != got.Up || got.Reason != c.want {
			t.Errorf("%s: want %q, got %+v", c.name, c.want, got)
		}
	}
	if got := Decide(Policy{}, Signals{}, up, now); !got.Up {
		t.Fatal("failover disabled keeps the vendor in routing")
	}

	// Recovery: a slow vendor stays out for the cooldown even with clean
	// signals; a down vendor returns as soon as binds are back.
	slow := State{Reason: "slow", Since: now.Add(-10 * time.Second).UnixMilli()}
	clean := Signals{Reports: []Report{rep(2, 2, 0, 0, 0)}}
	if got := Decide(pol, clean, slow, now); got.Up {
		t.Fatal("cooldown must hold")
	}
	if got := Decide(pol, clean, slow, now.Add(25*time.Second)); !got.Up {
		t.Fatal("after cooldown it comes back")
	}
	down := State{Reason: "down", Since: now.Add(-time.Second).UnixMilli()}
	if got := Decide(pol, clean, down, now); !got.Up {
		t.Fatal("binds back: in routing at once")
	}
	// "since" only moves on a change.
	if got := Decide(pol, Signals{Reports: []Report{rep(2, 2, 100, 0, 2500)}}, slow, now); got.Since != slow.Since {
		t.Fatal("unchanged verdict keeps its since")
	}
}

func TestRecorder(t *testing.T) {
	var r Recorder
	for i := range 100 {
		r.Submit(time.Duration(i)*time.Millisecond, nil, false)
	}
	r.Submit(0, errors.New("timeout"), true)
	r.Submit(0, errors.New("invalid number"), false) // the vendor answered
	r.Rerouted()
	s := r.Snapshot(1, 2)
	if s.OK != 101 || s.Failed != 1 || s.Rerouted != 1 || s.P95Ms != 95 || s.BindsUp != 1 || s.Binds != 2 {
		t.Fatalf("%+v", s)
	}
}

func TestEvaluate(t *testing.T) {
	addr := os.Getenv("REDIS_TEST_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	ctx := context.Background()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skipf("redis not reachable: %v", err)
	}
	t.Cleanup(func() { rdb.Close() })
	v1, v2, v3 := "evt-a-"+t.Name(), "evt-b-"+t.Name(), "evt-c-"+t.Name()
	t.Cleanup(func() {
		rdb.HDel(ctx, healthKey, v1+"|i1", v2+"|i1", v2+"|old")
		rdb.HDel(ctx, stateKey, v1, v2, v3)
		for i := range 15 {
			rdb.Del(ctx, dlrKey(v1, time.Now().Add(-time.Duration(i)*time.Minute)))
		}
	})
	_ = Publish(ctx, rdb, v1, "i1", rep(2, 2, 100, 0, 50))
	_ = Publish(ctx, rdb, v2, "i1", rep(0, 2, 0, 0, 0))
	b := []byte(`{"at":1}`) // instance gone long ago: ignored and cleaned up
	rdb.HSet(ctx, healthKey, v2+"|old", b)
	_ = CountDLRs(ctx, rdb, map[string][2]int{v1: {300, 100}})

	got, err := Evaluate(ctx, rdb, map[string]Policy{v1: pol, v2: pol, v3: pol}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got[v1].Up || got[v1].Reason != "dlr_rate" || got[v2].Reason != "down" || got[v3].Reason != "down" {
		t.Fatalf("verdicts: %+v", got)
	}
	pub, _ := States(ctx, rdb)
	if pub[v2].Reason != "down" {
		t.Fatalf("published: %+v", pub)
	}
	if rdb.HExists(ctx, healthKey, v2+"|old").Val() {
		t.Fatal("stale instance not cleaned up")
	}
}
