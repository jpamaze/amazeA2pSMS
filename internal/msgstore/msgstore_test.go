package msgstore

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/shopspring/decimal"

	"github.com/jpamaze/amazeA2pSMS/internal/domain"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	addr := os.Getenv("REDIS_TEST_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skipf("redis not reachable: %v", err)
	}
	t.Cleanup(func() { rdb.Close() })
	return New(rdb)
}

func TestLifecycleAndDuplicateDLRs(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	id := uuid.NewString()
	m := &domain.Message{ID: id, AccountID: "acct", Channel: domain.ChannelSMPP, Sender: "AMZTST",
		Destination: "919812345678", Segments: 2, Price: decimal.RequireFromString("0.2"),
		RegisteredDelivery: true, AcceptedAt: time.Now()}
	if err := s.Create(ctx, m); err != nil {
		t.Fatal(err)
	}
	if err := s.MapVendorIDs(ctx, "sim", id, []string{"v1", "v2"}, "0.1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Lookup(ctx, "sim", "v2"); got != id {
		t.Fatalf("lookup: %q", got)
	}

	changed, _ := s.Apply(ctx, id, domain.StatusSubmitted, "", time.Now())
	if !changed {
		t.Fatal("accepted -> submitted must change")
	}

	r, _ := s.RecordPart(ctx, id, "v1", true)
	r2, _ := s.RecordPart(ctx, id, "v1", true) // duplicate
	if !r.Counted || r2.Counted || r2.PartsDone != 1 {
		t.Fatalf("duplicate DLR counted: %+v %+v", r, r2)
	}
	r, _ = s.RecordPart(ctx, id, "v2", false)
	if r.Parts != 2 || r.PartsDone != 2 || r.PartsOK != 1 {
		t.Fatalf("parts: %+v", r)
	}

	if changed, _ := s.Apply(ctx, id, domain.StatusFailed, "034", time.Now()); !changed {
		t.Fatal("submitted -> failed must change")
	}
	// A late "submitted" (replayed event) must not move a final message back.
	if changed, _ := s.Apply(ctx, id, domain.StatusSubmitted, "", time.Now()); changed {
		t.Fatal("final status went backwards")
	}
	st, _ := s.Get(ctx, id)
	if st.Status != domain.StatusFailed || st.ErrorCode != "034" || st.DoneAt.IsZero() {
		t.Fatalf("state: %+v", st)
	}

	if st.ReceiptSent {
		t.Fatal("receipt not sent yet")
	}
	_ = s.MarkReceiptSent(ctx, id)
	if st, _ := s.Get(ctx, id); !st.ReceiptSent {
		t.Fatal("receipt mark lost")
	}
	if _, err := s.Apply(ctx, "missing-"+id, domain.StatusDelivered, "", time.Now()); err != ErrNotFound {
		t.Fatalf("missing record: %v", err)
	}
}

func TestReceiptQueue(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	acct := "acct-" + uuid.NewString()
	for i := range 3 {
		_ = s.EnqueueReceipt(ctx, acct, Receipt{MessageID: string(rune('a' + i)), Stat: "DELIVRD"})
	}
	got, err := s.ReadReceipts(ctx, acct, "node1", 10, 10*time.Millisecond)
	if err != nil || len(got) != 3 {
		t.Fatalf("read: %d %v", len(got), err)
	}
	// node1 acks one and dies; node2 claims the other two after they go stale.
	_ = s.AckReceipts(ctx, acct, got[0].EntryID)
	time.Sleep(30 * time.Millisecond)
	claimed, err := s.ClaimStaleReceipts(ctx, acct, "node2", 20*time.Millisecond, 10)
	if err != nil || len(claimed) != 2 || claimed[0].MessageID != "b" {
		t.Fatalf("claim: %+v %v", claimed, err)
	}
	s.rdb.Del(ctx, receiptStream(acct))
}
