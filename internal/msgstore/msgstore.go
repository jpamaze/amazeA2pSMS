// Package msgstore keeps in-flight message state in Redis: the message
// record, the vendor message ID mapping used to match DLRs, and the
// per-account queue of delivery receipts waiting for the customer.
//
// Records expire after TTL (72 h, the longest vendor DLR timeout). Long-term
// history goes to ClickHouse (next slice).
package msgstore

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/jpamaze/amazeA2pSMS/internal/domain"
)

// TTL is how long in-flight state is kept. It is longer than the default DLR
// timeout so the reconciler can still expire a message before its record goes.
const TTL = 96 * time.Hour

// ErrNotFound means no record (expired or never created).
var ErrNotFound = errors.New("msgstore: not found")

// Store is the Redis-backed message state store.
type Store struct {
	rdb redis.UniversalClient
}

// New returns a Store.
func New(rdb redis.UniversalClient) *Store { return &Store{rdb: rdb} }

func msgKey(id string) string            { return "msg:{" + id + "}" }
func dlrSetKey(id string) string         { return "msg:{" + id + "}:dlrs" }
func vmidKey(vendor, vmid string) string { return "vmid:" + vendor + ":" + vmid }

// State is the stored view of one message.
type State struct {
	ID                 string
	AccountID          string
	Channel            domain.Channel
	ClientRef          string
	Sender             string
	Destination        string
	Segments           int
	Price              string
	Status             domain.MessageStatus
	ErrorCode          string
	VendorID           string
	Parts              int
	PartsDone          int
	PartsOK            int
	RegisteredDelivery bool
	AcceptedAt         time.Time
	DoneAt             time.Time
	ReceiptSent        bool // the customer's final receipt or webhook was queued
	MCC, MNC           string
	PEID, TemplateID   string
	VendorMsgID        string // first part's vendor ID
	Cost               string // vendor cost for the whole message
}

// Create stores a newly accepted message.
func (s *Store) Create(ctx context.Context, m *domain.Message) error {
	k := msgKey(m.ID)
	reg := "0"
	if m.RegisteredDelivery {
		reg = "1"
	}
	_, err := s.rdb.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.HSet(ctx, k,
			"account", m.AccountID, "channel", string(m.Channel), "ref", m.ClientRef,
			"src", m.Sender, "dst", m.Destination, "segs", m.Segments, "price", m.Price.String(),
			"status", string(domain.StatusAccepted), "reg", reg,
			"accepted", m.AcceptedAt.UnixMilli(),
			"mcc", m.MCC, "mnc", m.MNC, "pe", m.DLT.PEID, "tpl", m.DLT.TemplateID)
		p.Expire(ctx, k, TTL)
		return nil
	})
	return err
}

// Delete removes a record (used when acceptance is rolled back).
func (s *Store) Delete(ctx context.Context, id string) error {
	return s.rdb.Del(ctx, msgKey(id), dlrSetKey(id)).Err()
}

// Get loads a record.
func (s *Store) Get(ctx context.Context, id string) (State, error) {
	v, err := s.rdb.HGetAll(ctx, msgKey(id)).Result()
	if err != nil {
		return State{}, err
	}
	if len(v) == 0 {
		return State{}, ErrNotFound
	}
	atoi := func(k string) int { n, _ := strconv.Atoi(v[k]); return n }
	ms := func(k string) time.Time {
		n, _ := strconv.ParseInt(v[k], 10, 64)
		if n == 0 {
			return time.Time{}
		}
		return time.UnixMilli(n).UTC()
	}
	return State{
		ID: id, AccountID: v["account"], Channel: domain.Channel(v["channel"]), ClientRef: v["ref"],
		Sender: v["src"], Destination: v["dst"], Segments: atoi("segs"), Price: v["price"],
		Status: domain.MessageStatus(v["status"]), ErrorCode: v["err"], VendorID: v["vendor"],
		Parts: atoi("parts"), PartsDone: atoi("parts_done"), PartsOK: atoi("parts_ok"),
		RegisteredDelivery: v["reg"] == "1", AcceptedAt: ms("accepted"), DoneAt: ms("done"),
		ReceiptSent: v["dlr_sent"] == "1",
		MCC:         v["mcc"], MNC: v["mnc"], PEID: v["pe"], TemplateID: v["tpl"],
		VendorMsgID: v["vmid"], Cost: v["cost"],
	}, nil
}

// MapVendorIDs records which vendor message IDs belong to a message, and how
// many parts it was sent as. It must run before the submitted status is
// published so a fast DLR can always be matched.
func (s *Store) MapVendorIDs(ctx context.Context, vendorID, msgID string, vmids []string, cost string) error {
	_, err := s.rdb.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.HSet(ctx, msgKey(msgID), "vendor", vendorID, "parts", len(vmids), "vmid", vmids[0], "cost", cost)
		for _, v := range vmids {
			p.Set(ctx, vmidKey(vendorID, v), msgID, TTL)
		}
		return nil
	})
	return err
}

// Lookup finds the message for a vendor message ID.
func (s *Store) Lookup(ctx context.Context, vendorID, vmid string) (string, error) {
	id, err := s.rdb.Get(ctx, vmidKey(vendorID, vmid)).Result()
	if errors.Is(err, redis.Nil) {
		return "", ErrNotFound
	}
	return id, err
}

//go:embed transition.lua
var transitionSrc string
var transitionScript = redis.NewScript(transitionSrc)

// Apply also maintains the in-flight index used by reconciliation.

// rank orders statuses; a status never moves to a lower rank.
func rank(st domain.MessageStatus) int {
	switch st {
	case domain.StatusAccepted:
		return 0
	case domain.StatusSubmitted:
		return 1
	}
	if st.Final() {
		return 2
	}
	return 0
}

// Apply moves a message to status if that is forward progress. It returns
// whether the status changed. Final statuses also record the done time.
func (s *Store) Apply(ctx context.Context, id string, st domain.MessageStatus, errCode string, at time.Time) (bool, error) {
	res, err := transitionScript.Run(ctx, s.rdb, []string{msgKey(id)},
		string(st), rank(st), errCode, at.UnixMilli(), boolInt(st.Final())).Int()
	if errors.Is(err, redis.Nil) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, err
	}
	if res < 0 {
		return false, ErrNotFound
	}
	if res == 1 {
		// Kept in its own keys (different cluster slots), so not in the script.
		switch {
		case st == domain.StatusSubmitted:
			err = s.TrackInflight(ctx, id, at)
		case st.Final():
			err = s.UntrackInflight(ctx, id)
		}
	}
	return res == 1, err
}

//go:embed dlr.lua
var dlrSrc string
var dlrScript = redis.NewScript(dlrSrc)

// PartResult is the count of receipts for a message after recording one.
type PartResult struct {
	Counted   bool // false if this vendor ID was already counted (duplicate DLR)
	Parts     int
	PartsDone int
	PartsOK   int
}

// RecordPart counts one part's receipt once, however often it is delivered.
func (s *Store) RecordPart(ctx context.Context, id, vmid string, delivered bool) (PartResult, error) {
	res, err := dlrScript.Run(ctx, s.rdb, []string{msgKey(id), dlrSetKey(id)},
		vmid, boolInt(delivered), int64(TTL.Seconds())).Int64Slice()
	if err != nil {
		return PartResult{}, err
	}
	if res[0] < 0 {
		return PartResult{}, ErrNotFound
	}
	return PartResult{Counted: res[0] == 1, Parts: int(res[1]), PartsDone: int(res[2]), PartsOK: int(res[3])}, nil
}

// MarkReceiptSent records that the final receipt was queued. It is set after
// queuing, so a crash in between can repeat a receipt but never lose one.
func (s *Store) MarkReceiptSent(ctx context.Context, ids ...string) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := s.rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
		for _, id := range ids {
			p.HSet(ctx, msgKey(id), "dlr_sent", 1)
		}
		return nil
	})
	return err
}

// SentBy returns, for each message ID, the vendor it was already submitted
// to ("" if none), in one round trip. The connector uses it to skip
// re-sending replayed records.
func (s *Store) SentBy(ctx context.Context, ids []string) ([]string, error) {
	cmds := make([]*redis.SliceCmd, len(ids))
	_, err := s.rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
		for i, id := range ids {
			cmds[i] = p.HMGet(ctx, msgKey(id), "vendor", "parts")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make([]string, len(ids))
	for i, c := range cmds {
		v := c.Val()
		if len(v) == 2 && v[0] != nil && v[1] != nil && v[1] != "0" {
			out[i], _ = v[0].(string)
		}
	}
	return out, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// String is for logs.
func (st State) String() string {
	return fmt.Sprintf("%s[%s %s parts %d/%d ok %d]", st.ID, st.Status, st.VendorID, st.PartsDone, st.Parts, st.PartsOK)
}
