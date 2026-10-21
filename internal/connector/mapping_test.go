package connector

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/shopspring/decimal"

	"github.com/jpamaze/amazeA2pSMS/internal/domain"
	"github.com/jpamaze/amazeA2pSMS/internal/msgstore"
)

// A message the vendor accepted while Redis was failing over (its record
// lost, Redis unreachable for a moment) still gets its record and its
// vendor ID mapping, so the receipt matches.
func TestMappingRidesOutARedisFailover(t *testing.T) {
	addr := os.Getenv("REDIS_TEST_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}
	direct := redis.NewClient(&redis.Options{Addr: addr})
	if err := direct.Ping(context.Background()).Err(); err != nil {
		t.Skipf("redis not reachable: %v", err)
	}
	defer direct.Close()

	// A port that refuses connections for 600 ms, then forwards to Redis.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().String()
	l.Close()
	go func() {
		time.Sleep(600 * time.Millisecond)
		l, err := net.Listen("tcp", port)
		if err != nil {
			return
		}
		defer l.Close()
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				r, err := net.Dial("tcp", addr)
				if err != nil {
					c.Close()
					return
				}
				go func() { _, _ = io.Copy(r, c); r.Close() }()
				_, _ = io.Copy(c, r)
				c.Close()
			}()
		}
	}()
	rdb := redis.NewClient(&redis.Options{Addr: port, MaxRetries: -1})
	defer rdb.Close()

	c := &Connector{VendorID: "v1", Store: msgstore.New(rdb), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	m := &domain.Message{ID: uuid.NewString(), AccountID: uuid.NewString(), Channel: domain.ChannelSMPP,
		Destination: "919812345678", Segments: 1, Price: decimal.RequireFromString("0.1"), AcceptedAt: time.Now()}
	ctx := context.Background()
	if err := c.mapVendorIDs(ctx, m, []string{"VM-" + m.ID}, "0.05"); err != nil {
		t.Fatalf("mapping: %v", err)
	}
	store := msgstore.New(direct)
	if id, err := store.Lookup(ctx, "v1", "VM-"+m.ID); err != nil || id != m.ID {
		t.Fatalf("lookup: %q %v", id, err)
	}
	if st, err := store.Get(ctx, m.ID); err != nil || st.AccountID != m.AccountID || st.VendorID != "v1" {
		t.Fatalf("record: %+v %v", st, err)
	}
	_ = store.Delete(ctx, m.ID)
	direct.Del(ctx, "vmid:v1:VM-"+m.ID)
}
