package dlr

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/shopspring/decimal"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/jpamaze/amazeA2pSMS/internal/domain"
	"github.com/jpamaze/amazeA2pSMS/internal/msgstore"
)

// A batch that failed after counting a receipt (Redis failing over) is
// retried: the replayed receipt must still finish the message and queue the
// customer's receipt, not be skipped as a duplicate.
func TestReplayedReceiptStillNotifies(t *testing.T) {
	addr := os.Getenv("REDIS_TEST_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	ctx := context.Background()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skipf("redis not reachable: %v", err)
	}
	defer rdb.Close()
	store := msgstore.New(rdb)
	p := &Processor{Store: store, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	m := &domain.Message{ID: uuid.NewString(), AccountID: uuid.NewString(), Channel: domain.ChannelSMPP, Sender: "AMZTST",
		Destination: "919812345678", Segments: 1, Price: decimal.RequireFromString("0.12"), RegisteredDelivery: true, AcceptedAt: time.Now()}
	vmid := "VM-" + m.ID
	defer func() { _ = store.Delete(ctx, m.ID); rdb.Del(ctx, "vmid:sim:"+vmid) }()
	if err := store.Create(ctx, m); err != nil {
		t.Fatal(err)
	}
	if err := store.MapVendorIDs(ctx, "sim", m.ID, []string{vmid}, "0.05"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Apply(ctx, m.ID, domain.StatusSubmitted, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	// The first attempt counted the part, then failed.
	if _, err := store.RecordPart(ctx, m.ID, vmid, true); err != nil {
		t.Fatal(err)
	}

	var ob outbox
	d := domain.DLR{VendorID: "sim", VendorMsgID: vmid, Stat: "DELIVRD", ReceivedAt: time.Now()}
	if _, err := p.receipt(ctx, &kgo.Record{}, d, &ob); err != nil {
		t.Fatal(err)
	}
	st, _ := store.Get(ctx, m.ID)
	if st.Status != domain.StatusDelivered || len(ob.marks) != 1 || ob.marks[0] != m.ID || len(ob.recs) == 0 {
		t.Fatalf("replay: status %s, marks %v, %d records", st.Status, ob.marks, len(ob.recs))
	}
	// Once the receipt went out, a vendor's duplicate receipt queues nothing.
	if err := store.MarkReceiptSent(ctx, m.ID); err != nil {
		t.Fatal(err)
	}
	var again outbox
	if _, err := p.receipt(ctx, &kgo.Record{}, d, &again); err != nil || len(again.marks) != 0 || len(again.recs) != 0 {
		t.Fatalf("duplicate after notify: %v %v %d", err, again.marks, len(again.recs))
	}
}
