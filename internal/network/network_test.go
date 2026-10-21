package network

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/jpamaze/amazeA2pSMS/internal/platform/pgtest"
)

func rdb(t *testing.T) redis.UniversalClient {
	t.Helper()
	addr := os.Getenv("REDIS_TEST_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}
	c := redis.NewClient(&redis.Options{Addr: addr})
	if err := c.Ping(context.Background()).Err(); err != nil {
		t.Skipf("redis not reachable: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestSeriesAndMNP(t *testing.T) {
	r := rdb(t)
	pool := pgtest.Pool(t)
	ctx := context.Background()
	for _, row := range [][3]string{{"98", "404", "10"}, {"98100", "404", "45"}, {"70", "405", "861"}} {
		if _, err := pool.Exec(ctx, `INSERT INTO number_series (prefix, mcc, mnc) VALUES ($1, $2, $3)`, row[0], row[1], row[2]); err != nil {
			t.Fatal(err)
		}
	}
	s := &Series{PG: pool}
	if err := s.Load(ctx); err != nil {
		t.Fatal(err)
	}
	r.Del(ctx, "mnp:98100123", "mnp:98200123", "mnp:70123456") // shared Redis: start clean
	res := &Resolver{Series: s, Redis: r}
	check := func(e164, mcc, mnc, src string) {
		t.Helper()
		n, err := res.Resolve(ctx, e164)
		if err != nil || n.MCC != mcc || n.MNC != mnc || n.Source != src {
			t.Fatalf("%s: want %s/%s (%s), got %+v %v", e164, mcc, mnc, src, n, err)
		}
	}
	check("919810012345", "404", "45", "series") // longest prefix wins
	check("919820012345", "404", "10", "series")
	check("917012345678", "405", "861", "series")
	check("916012345678", "404", "*", "default")

	csv := "msisdn,mcc,mnc\n+91 98100 12345,405,861\n9820012345,404,45\nnot-a-number,404,45\n7012345678,404,\n"
	t.Cleanup(func() { r.Del(ctx, "mnp:98100123", "mnp:98200123", "mnp:70123456") })
	n, bad, err := ImportMNP(ctx, r, strings.NewReader(csv))
	if err != nil || n != 3 || len(bad) != 1 {
		t.Fatalf("import: %d %v %v", n, bad, err)
	}
	check("919810012345", "405", "861", "mnp") // ported away
	check("919810012346", "404", "45", "series")
	check("919820012345", "404", "45", "mnp")
	check("917012345678", "405", "861", "series") // empty mnc removed it

	// Redis down: the series answer still comes back, with the error.
	broken := &Resolver{Series: s, Redis: redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1})}
	if n, err := broken.Resolve(ctx, "919810012345"); err == nil || n.Source != "series" {
		t.Fatalf("redis down: %+v %v", n, err)
	}
}

func TestHLR(t *testing.T) {
	r := rdb(t)
	ctx := context.Background()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls.Add(1)
		if req.Header.Get("X-Key") != "k" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch req.URL.Query().Get("n") {
		case "919810000001":
			w.Write([]byte(`{"result":{"mccmnc":"405861","state":"DELIVERABLE"}}`))
		case "919810000002":
			w.Write([]byte(`{"result":{"mccmnc":"40445","state":"UNKNOWN_SUBSCRIBER"}}`))
		default:
			w.Write([]byte(`{"result":{"state":"???"}}`))
		}
	}))
	t.Cleanup(srv.Close)
	h := &HLR{Redis: r, Cfg: HLRConfig{URL: srv.URL + "/?n={msisdn}", AuthHeader: "X-Key", AuthValue: "k",
		MCCMNCPath: "result.mccmnc", StatusPath: "result.state", StatusOK: []string{"deliverable"},
		StatusInvalid: []string{"unknown_subscriber"}, StatusAbsent: []string{"absent"}, CacheTTL: time.Minute}}
	nums := []string{"919810000001", "919810000002", "919810000003"}
	t.Cleanup(func() { r.Del(ctx, "hlr:919810000001", "hlr:919810000002", "hlr:919810000003") })

	if got, _ := h.Lookup(ctx, nums[0]); got.Status != HLROK || got.MCC != "405" || got.MNC != "861" {
		t.Fatalf("ok: %+v", got)
	}
	if got, _ := h.Lookup(ctx, nums[1]); got.Status != HLRInvalid {
		t.Fatalf("invalid: %+v", got)
	}
	if got, _ := h.Lookup(ctx, nums[2]); got.Status != HLRUnknown || got.MCC != "" {
		t.Fatalf("unknown: %+v", got)
	}
	c, err := h.Cached(ctx, nums)
	if err != nil || len(c) != 2 || !c[nums[0]].Cached || c[nums[0]].MNC != "861" {
		t.Fatalf("cache (unknown answers are not cached): %+v %v", c, err)
	}
	h.Cfg.AuthValue = "wrong"
	if _, err := h.Lookup(ctx, nums[0]); err == nil {
		t.Fatal("HTTP 401 must be an error")
	}
	if calls.Load() != 4 {
		t.Fatalf("provider calls: %d", calls.Load())
	}
}
