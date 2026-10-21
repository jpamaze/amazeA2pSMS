package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/jpamaze/amazeA2pSMS/internal/platform/metrics"

	"github.com/shopspring/decimal"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/jpamaze/amazeA2pSMS/internal/domain"
	"github.com/jpamaze/amazeA2pSMS/internal/msgstore"
	"github.com/jpamaze/amazeA2pSMS/internal/numbering"
	"github.com/jpamaze/amazeA2pSMS/internal/platform/kafka"
	"github.com/jpamaze/amazeA2pSMS/internal/smpp/gsm7"
	"github.com/jpamaze/amazeA2pSMS/internal/smpp/pdu"
	"github.com/jpamaze/amazeA2pSMS/internal/smpp/session"
)

// MaxAttempts is how often one request is tried on temporary errors.
const MaxAttempts = 5

// Transport sends one message to the vendor over a protocol. It retries
// temporary errors itself and returns the vendor message IDs it got (one per
// part). On error, IDs of parts already accepted are still returned.
type Transport interface {
	Send(ctx context.Context, m *domain.Message) ([]string, error)
	// Window is how many messages may be in flight at once.
	Window() int
}

// SendError is a vendor refusal with a platform error code.
type SendError struct {
	Code      string
	Temporary bool
	Err       error
}

func (e *SendError) Error() string { return e.Code + ": " + e.Err.Error() }
func (e *SendError) Unwrap() error { return e.Err }

// Connector sends one vendor's routed messages.
type Connector struct {
	VendorID  string
	Transport Transport
	Store     *msgstore.Store
	Kafka     *kgo.Client
	Log       *slog.Logger
}

// Process sends a batch of routed records concurrently and publishes one
// status event per message. It returns only when every message has a final
// outcome for this hop (submitted or failed), so offsets can be committed.
func (c *Connector) Process(ctx context.Context, recs []*kgo.Record) error {
	msgs := make([]*domain.Message, len(recs))
	ids := make([]string, 0, len(recs))
	for i, rec := range recs {
		var m domain.Message
		if err := json.Unmarshal(rec.Value, &m); err != nil {
			c.Log.Error("bad route record; skipping", "offset", rec.Offset, "err", err)
			continue
		}
		msgs[i] = &m
		ids = append(ids, m.ID)
	}
	// One round trip for the whole batch: which were already sent (replay)?
	sentBy := map[string]string{}
	if vendors, err := c.Store.SentBy(ctx, ids); err == nil {
		for i, v := range vendors {
			sentBy[ids[i]] = v
		}
	}

	sem := make(chan struct{}, c.Transport.Window())
	events := make([]*domain.StatusEvent, len(recs))
	var wg sync.WaitGroup
	for i, mp := range msgs {
		if mp == nil {
			continue
		}
		m := *mp
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}
		wg.Add(1)
		go func(i int, m domain.Message) {
			defer wg.Done()
			defer func() { <-sem }()
			events[i] = c.send(ctx, &m, sentBy[m.ID])
		}(i, m)
	}
	wg.Wait()
	if ctx.Err() != nil {
		return ctx.Err() // do not commit a half-sent batch
	}

	out := make([]*kgo.Record, 0, len(events))
	for _, ev := range events {
		if ev == nil {
			continue
		}
		r, err := kafka.Record(kafka.TopicStatus, ev.MessageID, ev)
		if err != nil {
			return err
		}
		out = append(out, r)
	}
	return kafka.ProduceJSON(ctx, c.Kafka, out...)
}

func (c *Connector) send(ctx context.Context, m *domain.Message, sentTo string) *domain.StatusEvent {
	ev := &domain.StatusEvent{
		MessageID: m.ID, AccountID: m.AccountID, HoldID: m.HoldID, Price: m.Price, Charges: m.Charges, VendorID: c.VendorID,
	}
	// A replayed record (connector restarted after sending but before its
	// offset was committed) is not sent twice.
	if sentTo == c.VendorID {
		ev.Status, ev.At = domain.StatusSubmitted, time.Now().UTC()
		return ev
	}
	start := time.Now()
	vmids, err := c.Transport.Send(ctx, m)
	metrics.VendorSubmitSeconds.WithLabelValues(c.VendorID).Observe(time.Since(start).Seconds())
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		ev.Status, ev.ErrorCode, ev.At = domain.StatusFailed, errorCode(err), time.Now().UTC()
		c.Log.Info("vendor refused message", "message", m.ID, "err", err)
	}
	if len(vmids) > 0 {
		// Mapping first, so a fast DLR always finds its message.
		cost := ""
		if m.Route != nil {
			cost = m.Route.Cost.Mul(decimal.NewFromInt(int64(m.Segments))).String()
		}
		if err := c.Store.MapVendorIDs(ctx, c.VendorID, m.ID, vmids, cost); err != nil {
			c.Log.Error("vendor ID mapping failed; DLRs for this message will be unmatched",
				"message", m.ID, "err", err)
		}
	}
	ev.VendorMsgIDs = vmids
	if ev.Status == "" {
		ev.Status, ev.At = domain.StatusSubmitted, time.Now().UTC()
	}
	metrics.VendorSubmits.WithLabelValues(c.VendorID, string(ev.Status)).Inc()
	return ev
}

// withRetry runs try up to MaxAttempts times while isTemp says the error is
// temporary, backing off with jitter.
func withRetry(ctx context.Context, isTemp func(error) bool, try func() (string, error)) (string, error) {
	backoff := 200 * time.Millisecond
	var last error
	for attempt := 1; attempt <= MaxAttempts; attempt++ {
		id, err := try()
		if err == nil {
			return id, nil
		}
		last = err
		if !isTemp(err) || ctx.Err() != nil {
			return "", err
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(backoff + time.Duration(rand.Int64N(int64(backoff/2)))):
		}
		backoff = min(backoff*2, 5*time.Second)
	}
	return "", last
}

// SMPPTransport sends over a pool of SMPP binds.
type SMPPTransport struct {
	Pool *Pool
	TLVs TLVProfile
}

// Window implements Transport.
func (t *SMPPTransport) Window() int { return t.Pool.Window() }

// Send implements Transport: one submit_sm per part.
func (t *SMPPTransport) Send(ctx context.Context, m *domain.Message) ([]string, error) {
	var vmids []string
	for _, sm := range t.build(m) {
		id, err := withRetry(ctx, temporary, func() (string, error) {
			b, err := t.Pool.Pick(ctx)
			if err != nil {
				return "", err
			}
			return b.Submit(ctx, sm)
		})
		if err != nil {
			return vmids, err
		}
		vmids = append(vmids, id)
	}
	return vmids, nil
}

// temporary reports whether a submit error is worth retrying. A timeout is
// retried even though the vendor may have accepted the first attempt; a rare
// duplicate is preferred to a lost message.
func temporary(err error) bool {
	var se *session.StatusError
	if errors.As(err, &se) {
		switch se.Status {
		case pdu.StatusThrottled, pdu.StatusMsgQFul, pdu.StatusSysErr:
			return true
		}
		return false
	}
	return errors.Is(err, session.ErrClosed) || errors.Is(err, session.ErrTimeout) || errors.Is(err, errNotBound)
}

func errorCode(err error) string {
	var se *session.StatusError
	if errors.As(err, &se) {
		return fmt.Sprintf("SMPP_%08X", se.Status)
	}
	var send *SendError
	if errors.As(err, &send) {
		return send.Code
	}
	return "VENDOR_UNREACHABLE"
}

// build turns a message into one submit_sm per part.
func (t *SMPPTransport) build(m *domain.Message) []*pdu.SM {
	base := pdu.SM{
		Dest: m.Destination, DestTON: 1, DestNPI: 1,
		Source: m.Sender, RegisteredDelivery: 1,
	}
	if numbering.IsNumeric(m.Sender) {
		base.SourceTON, base.SourceNPI = 1, 1
	} else {
		base.SourceTON, base.SourceNPI = 5, 0
	}
	t.addDLT(&base, m.DLT)

	if len(m.UDH) > 0 {
		sm := base
		coding, enc := encodeWhole(m.Text)
		sm.DataCoding = coding
		sm.ESMClass = pdu.ESMUDHI
		sm.ShortMessage = append(append([]byte(nil), m.UDH...), enc...)
		return []*pdu.SM{&sm}
	}

	coding, parts := gsm7.Split(m.Text)
	dc := byte(0x00)
	if coding == gsm7.CodingUCS2 {
		dc = 0x08
	}
	if len(parts) == 1 {
		sm := base
		sm.DataCoding = dc
		sm.ShortMessage = parts[0]
		return []*pdu.SM{&sm}
	}
	ref := byte(rand.IntN(256))
	out := make([]*pdu.SM, len(parts))
	for i, p := range parts {
		sm := base
		sm.TLVs = append(pdu.TLVs(nil), base.TLVs...)
		sm.DataCoding = dc
		sm.ESMClass = pdu.ESMUDHI
		udh := []byte{0x05, 0x00, 0x03, ref, byte(len(parts)), byte(i + 1)}
		sm.ShortMessage = append(udh, p...)
		out[i] = &sm
	}
	return out
}

func encodeWhole(text string) (byte, []byte) {
	if enc, err := gsm7.Encode(text); err == nil {
		return 0x00, enc
	}
	return 0x08, gsm7.EncodeUCS2(text)
}

func (t *SMPPTransport) addDLT(sm *pdu.SM, f domain.DLTFields) {
	set := func(tag uint16, v string) {
		if tag != 0 && v != "" {
			sm.TLVs.SetString(tag, v)
		}
	}
	set(t.TLVs.PEID, f.PEID)
	set(t.TLVs.TemplateID, f.TemplateID)
	set(t.TLVs.TMChain, f.TMChain)
}

// Deliver journals a vendor deliver_sm to Kafka before it is acknowledged
// (blueprint rule 3). If Kafka is unavailable the vendor gets an error and
// redelivers, so no receipt is lost.
func (c *Connector) Deliver(ctx context.Context, sm *pdu.SM) uint32 {
	var rec *kgo.Record
	var err error
	if sm.IsReceipt() {
		r, perr := pdu.ParseReceipt(sm)
		if perr != nil {
			c.Log.Warn("unparseable receipt acknowledged and dropped", "err", perr)
			return pdu.StatusOK
		}
		metrics.VendorDLRs.WithLabelValues(c.VendorID, r.Stat).Inc()
		rec, err = kafka.Record(kafka.TopicDLR, c.VendorID+":"+r.ID, domain.DLR{
			VendorID: c.VendorID, VendorMsgID: r.ID, Stat: r.Stat, ErrorCode: r.Err,
			DoneAt: r.DoneDate, ReceivedAt: time.Now().UTC(),
		})
	} else {
		text := string(sm.Payload())
		if sm.DataCoding == 0x08 {
			text = gsm7.DecodeUCS2(sm.Payload())
		}
		rec, err = kafka.Record(kafka.TopicMO, sm.Source, domain.MO{
			VendorID: c.VendorID, From: sm.Source, To: sm.Dest, Text: text, ReceivedAt: time.Now().UTC(),
		})
	}
	if err == nil {
		err = kafka.ProduceJSON(ctx, c.Kafka, rec)
	}
	if err != nil {
		c.Log.Error("could not journal deliver_sm; vendor will retry", "err", err)
		return pdu.StatusSysErr
	}
	return pdu.StatusOK
}

// JournalDLR writes a vendor receipt to Kafka. The caller acknowledges the
// vendor only when it returns nil.
func (c *Connector) JournalDLR(ctx context.Context, d domain.DLR) error {
	d.VendorID = c.VendorID
	metrics.VendorDLRs.WithLabelValues(c.VendorID, d.Stat).Inc()
	rec, err := kafka.Record(kafka.TopicDLR, c.VendorID+":"+d.VendorMsgID, d)
	if err != nil {
		return err
	}
	return kafka.ProduceJSON(ctx, c.Kafka, rec)
}
