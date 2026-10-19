// Package dlr turns status events and vendor receipts into message state and
// customer delivery receipts.
package dlr

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/jpamaze/amazeA2pSMS/internal/domain"
	"github.com/jpamaze/amazeA2pSMS/internal/msgstore"
	"github.com/jpamaze/amazeA2pSMS/internal/platform/kafka"
)

// MatchWindow is how long an unmatched DLR is re-queued waiting for its
// vendor ID mapping before it is dropped as unknown.
const MatchWindow = 2 * time.Minute

// Processor handles sms.status and sms.dlr records.
type Processor struct {
	Store   *msgstore.Store
	Kafka   *kgo.Client
	Log     *slog.Logger
	Workers int
}

// Process handles a batch with a worker pool. State transitions are
// monotonic and receipts are claimed once, so order across records does not matter.
func (p *Processor) Process(ctx context.Context, recs []*kgo.Record) error {
	workers := p.Workers
	if workers <= 0 {
		workers = 32
	}
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
		requeue  []*kgo.Record
	)
	jobs := make(chan *kgo.Record)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for rec := range jobs {
				again, err := p.handle(ctx, rec)
				mu.Lock()
				if err != nil && firstErr == nil {
					firstErr = err
				}
				if again != nil {
					requeue = append(requeue, again)
				}
				mu.Unlock()
			}
		}()
	}
	for _, r := range recs {
		jobs <- r
	}
	close(jobs)
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	if len(requeue) > 0 {
		time.Sleep(100 * time.Millisecond) // give in-flight mappings a moment
		return kafka.ProduceJSON(ctx, p.Kafka, requeue...)
	}
	return nil
}

// handle returns a record to re-queue (unmatched DLR) or an infrastructure error.
func (p *Processor) handle(ctx context.Context, rec *kgo.Record) (*kgo.Record, error) {
	switch rec.Topic {
	case kafka.TopicStatus:
		var ev domain.StatusEvent
		if err := json.Unmarshal(rec.Value, &ev); err != nil {
			p.Log.Error("bad status record; skipping", "offset", rec.Offset, "err", err)
			return nil, nil
		}
		return nil, p.status(ctx, ev)
	case kafka.TopicDLR:
		var d domain.DLR
		if err := json.Unmarshal(rec.Value, &d); err != nil {
			p.Log.Error("bad dlr record; skipping", "offset", rec.Offset, "err", err)
			return nil, nil
		}
		return p.receipt(ctx, rec, d)
	}
	return nil, nil
}

func (p *Processor) status(ctx context.Context, ev domain.StatusEvent) error {
	changed, err := p.Store.Apply(ctx, ev.MessageID, ev.Status, ev.ErrorCode, ev.At)
	if errors.Is(err, msgstore.ErrNotFound) {
		return nil // expired record; nothing to update
	}
	if err != nil {
		return err
	}
	if changed && ev.Status.Final() {
		return p.notify(ctx, ev.MessageID, ev.Status, ev.ErrorCode)
	}
	return nil
}

func (p *Processor) receipt(ctx context.Context, rec *kgo.Record, d domain.DLR) (*kgo.Record, error) {
	msgID, err := p.Store.Lookup(ctx, d.VendorID, d.VendorMsgID)
	if errors.Is(err, msgstore.ErrNotFound) {
		if time.Since(d.ReceivedAt) > MatchWindow {
			p.Log.Warn("unmatched DLR dropped", "vendor", d.VendorID, "vendor_msg_id", d.VendorMsgID)
			return nil, nil
		}
		d.Attempt++
		again, err := kafka.Record(kafka.TopicDLR, string(rec.Key), d)
		return again, err
	}
	if err != nil {
		return nil, err
	}

	res, err := p.Store.RecordPart(ctx, msgID, d.VendorMsgID, d.Stat == "DELIVRD")
	if errors.Is(err, msgstore.ErrNotFound) {
		return nil, nil
	}
	if err != nil || !res.Counted || res.Parts == 0 || res.PartsDone < res.Parts {
		return nil, err
	}

	final := domain.StatusDelivered
	code := ""
	if res.PartsOK < res.Parts {
		final = domain.StatusFailed
		if d.Stat == "EXPIRED" {
			final = domain.StatusExpired
		}
		code = d.Stat
		if d.ErrorCode != "" {
			code = d.Stat + ":" + d.ErrorCode
		}
	}
	// Receipt dates have minute resolution; the time we received the final
	// receipt is the accurate completion time.
	at := d.ReceivedAt
	changed, err := p.Store.Apply(ctx, msgID, final, code, at)
	if err != nil || !changed {
		return nil, ignoreNotFound(err)
	}
	return nil, p.notify(ctx, msgID, final, code)
}

// Reconcile expires messages that have waited longer than timeout for a
// DLR (blueprint rule 9) and sends the customer an EXPIRED receipt. It runs
// until ctx is cancelled; several replicas may run it safely because state
// transitions and receipt claims are idempotent.
func (p *Processor) Reconcile(ctx context.Context, timeout, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		for ctx.Err() == nil {
			ids, err := p.Store.Overdue(ctx, time.Now().Add(-timeout), 1000)
			if err != nil {
				p.Log.Warn("reconcile scan", "err", err)
				break
			}
			if len(ids) == 0 {
				break
			}
			for _, id := range ids {
				changed, err := p.Store.Apply(ctx, id, domain.StatusExpired, "NO_DLR", time.Now().UTC())
				switch {
				case errors.Is(err, msgstore.ErrNotFound) || (err == nil && !changed):
					_ = p.Store.UntrackInflight(ctx, id) // record gone or already final
				case err != nil:
					p.Log.Warn("reconcile apply", "message", id, "err", err)
				default:
					if err := p.notify(ctx, id, domain.StatusExpired, "NO_DLR"); err != nil {
						p.Log.Warn("reconcile notify", "message", id, "err", err)
					}
				}
			}
			p.Log.Info("expired messages without DLR", "count", len(ids))
		}
	}
}

// notify queues the customer's final receipt once, if they asked for one.
// HTTP customers read status by API now; webhooks come in the next slice.
func (p *Processor) notify(ctx context.Context, msgID string, st domain.MessageStatus, code string) error {
	s, err := p.Store.Get(ctx, msgID)
	if err != nil {
		return ignoreNotFound(err)
	}
	if s.Channel != domain.ChannelSMPP || !s.RegisteredDelivery {
		return nil
	}
	first, err := p.Store.ClaimReceipt(ctx, msgID)
	if err != nil || !first {
		return err
	}
	return p.Store.EnqueueReceipt(ctx, s.AccountID, msgstore.Receipt{
		MessageID: msgID, Stat: receiptStat(st), Err: numericErr(code),
		Sender: s.Sender, Dest: s.Destination, Submitted: s.AcceptedAt, Done: s.DoneAt,
	})
}

func receiptStat(st domain.MessageStatus) string {
	switch st {
	case domain.StatusDelivered:
		return "DELIVRD"
	case domain.StatusRejected:
		return "REJECTD"
	case domain.StatusExpired:
		return "EXPIRED"
	}
	return "UNDELIV"
}

// numericErr keeps the vendor's numeric error (after "STAT:") for the receipt.
func numericErr(code string) string {
	for i := len(code) - 1; i >= 0; i-- {
		if code[i] == ':' {
			return code[i+1:]
		}
	}
	for _, c := range code {
		if c < '0' || c > '9' {
			return ""
		}
	}
	return code
}

func ignoreNotFound(err error) error {
	if errors.Is(err, msgstore.ErrNotFound) {
		return nil
	}
	return err
}
