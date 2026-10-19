package msgstore

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// Receipt is a final delivery receipt waiting to be sent to a customer.
type Receipt struct {
	MessageID string
	Stat      string // DELIVRD, UNDELIV, REJECTD, EXPIRED
	Err       string
	Sender    string
	Dest      string
	Submitted time.Time
	Done      time.Time
}

// PendingReceipt is a queued receipt with its stream entry ID.
type PendingReceipt struct {
	EntryID string
	Receipt
}

const receiptGroup = "deliver"

func receiptStream(accountID string) string { return "custdlr:{" + accountID + "}" }

// EnqueueReceipt queues a receipt for the account's SMPP receiver binds.
// The stream lives in Redis (AOF) so receipts survive a restart of the
// SMPP server and are delivered when the customer binds again.
func (s *Store) EnqueueReceipt(ctx context.Context, accountID string, r Receipt) error {
	return s.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: receiptStream(accountID),
		Values: []any{
			"id", r.MessageID, "stat", r.Stat, "err", r.Err, "src", r.Sender, "dst", r.Dest,
			"sub", r.Submitted.UnixMilli(), "done", r.Done.UnixMilli(),
		},
	}).Err()
}

// ensureGroup creates the consumer group once per stream.
func (s *Store) ensureGroup(ctx context.Context, stream string) error {
	err := s.rdb.XGroupCreateMkStream(ctx, stream, receiptGroup, "0").Err()
	if err != nil && !strings.Contains(err.Error(), "BUSYGROUP") {
		return err
	}
	return nil
}

// ReadReceipts returns up to count new receipts for consumer, waiting up to block.
func (s *Store) ReadReceipts(ctx context.Context, accountID, consumer string, count int64, block time.Duration) ([]PendingReceipt, error) {
	stream := receiptStream(accountID)
	if err := s.ensureGroup(ctx, stream); err != nil {
		return nil, err
	}
	res, err := s.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group: receiptGroup, Consumer: consumer, Streams: []string{stream, ">"},
		Count: count, Block: block,
	}).Result()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []PendingReceipt
	for _, st := range res {
		out = append(out, toReceipts(st.Messages)...)
	}
	return out, nil
}

// ClaimStaleReceipts takes over receipts another consumer read but did not
// acknowledge within minIdle (its node died or the bind dropped).
func (s *Store) ClaimStaleReceipts(ctx context.Context, accountID, consumer string, minIdle time.Duration, count int64) ([]PendingReceipt, error) {
	stream := receiptStream(accountID)
	if err := s.ensureGroup(ctx, stream); err != nil {
		return nil, err
	}
	msgs, _, err := s.rdb.XAutoClaim(ctx, &redis.XAutoClaimArgs{
		Stream: stream, Group: receiptGroup, Consumer: consumer, MinIdle: minIdle, Start: "0", Count: count,
	}).Result()
	if err != nil {
		return nil, err
	}
	return toReceipts(msgs), nil
}

// AckReceipts removes delivered receipts.
func (s *Store) AckReceipts(ctx context.Context, accountID string, entryIDs ...string) error {
	if len(entryIDs) == 0 {
		return nil
	}
	stream := receiptStream(accountID)
	_, err := s.rdb.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.XAck(ctx, stream, receiptGroup, entryIDs...)
		p.XDel(ctx, stream, entryIDs...)
		return nil
	})
	return err
}

func toReceipts(msgs []redis.XMessage) []PendingReceipt {
	out := make([]PendingReceipt, 0, len(msgs))
	for _, m := range msgs {
		str := func(k string) string { v, _ := m.Values[k].(string); return v }
		ms := func(k string) time.Time {
			n, _ := strconv.ParseInt(str(k), 10, 64)
			return time.UnixMilli(n).UTC()
		}
		out = append(out, PendingReceipt{EntryID: m.ID, Receipt: Receipt{
			MessageID: str("id"), Stat: str("stat"), Err: str("err"), Sender: str("src"), Dest: str("dst"),
			Submitted: ms("sub"), Done: ms("done"),
		}})
	}
	return out
}
