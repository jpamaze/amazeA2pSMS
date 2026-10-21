// Package ingest is the acceptance pipeline shared by the SMPP and HTTP
// edges: validate, price, hold balance, record state, write to Kafka.
// A message is acknowledged to the customer only after Accept returns nil,
// which means it is durably in Kafka (blueprint rules 1, 2 and 4).
package ingest

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jpamaze/amazeA2pSMS/internal/platform/metrics"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/shopspring/decimal"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/jpamaze/amazeA2pSMS/internal/billing/hierarchy"
	"github.com/jpamaze/amazeA2pSMS/internal/billing/pricing"
	"github.com/jpamaze/amazeA2pSMS/internal/billing/wallet"
	"github.com/jpamaze/amazeA2pSMS/internal/domain"
	"github.com/jpamaze/amazeA2pSMS/internal/msgstore"
	"github.com/jpamaze/amazeA2pSMS/internal/numbering"
	"github.com/jpamaze/amazeA2pSMS/internal/platform/kafka"
	"github.com/jpamaze/amazeA2pSMS/internal/smpp/gsm7"
)

// Errors an edge maps to its own protocol codes.
var (
	ErrInvalidDestination = numbering.ErrInvalidDestination
	ErrInvalidSender      = numbering.ErrInvalidSender
	ErrEmptyMessage       = errors.New("ingest: empty message")
	ErrTooLong            = errors.New("ingest: message longer than the segment limit")
	ErrNoPrice            = pricing.ErrNoPrice
	ErrInsufficient       = wallet.ErrInsufficient
	ErrUnavailable        = errors.New("ingest: temporarily unavailable")
	// ErrResellerBalance means the sender can pay but its reseller cannot.
	ErrResellerBalance = errors.New("ingest: reseller balance does not cover the message")
	// ErrResellerNoPrice means the reseller has no rate for the destination.
	ErrResellerNoPrice = errors.New("ingest: reseller has no rate for destination")
	// ErrInactive means the account or its reseller is suspended.
	ErrInactive = hierarchy.ErrInactive
)

// MaxSegments caps one message.
const MaxSegments = 10

// Request is one message as an edge received it.
type Request struct {
	AccountID          string
	Channel            domain.Channel
	Sender             string
	Destination        string
	Text               string
	UDH                []byte // set when the customer sends pre-split parts
	DLT                domain.DLTFields
	RegisteredDelivery bool
	ClientRef          string
	IdempotencyKey     string // HTTP Idempotency-Key; empty for SMPP
}

// Result is what the edge returns to the customer.
type Result struct {
	MessageID string
	Segments  int
	Price     decimal.Decimal
	Duplicate bool // IdempotencyKey seen before; MessageID is the original
}

// Pipeline accepts messages.
type Pipeline struct {
	Chain   hierarchy.Chain // who pays; nil charges the sender only
	Pricer  pricing.Pricer
	Wallets *wallet.Store
	Store   *msgstore.Store
	Redis   redis.UniversalClient
	Kafka   *kgo.Client
	Timeout time.Duration // bound for the whole accept (default 3s)
}

// Accept validates, prices, holds and enqueues one message.
func (p *Pipeline) Accept(ctx context.Context, req Request) (Result, error) {
	start := time.Now()
	res, err := p.accept(ctx, req)
	ch := string(req.Channel)
	if err != nil {
		metrics.Refused.WithLabelValues(ch, Reason(err)).Inc()
		return res, err
	}
	metrics.Accepted.WithLabelValues(ch).Inc()
	metrics.AcceptSeconds.WithLabelValues(ch).Observe(time.Since(start).Seconds())
	return res, nil
}

// Reason is a short metrics label for an acceptance error.
func Reason(err error) string {
	switch {
	case errors.Is(err, ErrInvalidDestination):
		return "invalid_destination"
	case errors.Is(err, ErrInvalidSender):
		return "invalid_sender"
	case errors.Is(err, ErrEmptyMessage), errors.Is(err, ErrTooLong):
		return "invalid_text"
	case errors.Is(err, ErrNoPrice):
		return "no_rate"
	case errors.Is(err, ErrInsufficient):
		return "insufficient_balance"
	case errors.Is(err, ErrResellerBalance):
		return "reseller_balance"
	case errors.Is(err, ErrResellerNoPrice):
		return "reseller_no_rate"
	case errors.Is(err, ErrInactive):
		return "account_inactive"
	}
	return "unavailable"
}

func (p *Pipeline) accept(ctx context.Context, req Request) (Result, error) {
	timeout := p.Timeout
	if timeout == 0 {
		timeout = 3 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	dest, err := numbering.NormalizeIndia(req.Destination)
	if err != nil {
		return Result{}, err
	}
	if err := numbering.ValidateSender(req.Sender); err != nil {
		return Result{}, err
	}
	if req.Text == "" && len(req.UDH) == 0 {
		return Result{}, ErrEmptyMessage
	}
	segments := 1
	if len(req.UDH) == 0 {
		segments = gsm7.PlanFor(req.Text).Segments
	}
	if segments > MaxSegments {
		return Result{}, ErrTooLong
	}

	id, err := uuid.NewV7()
	if err != nil {
		return Result{}, err
	}
	msgID := id.String()

	idemKey := ""
	if req.IdempotencyKey != "" {
		idemKey = "idem:{" + req.AccountID + "}:" + req.IdempotencyKey
		prev, err := p.Redis.SetArgs(ctx, idemKey, msgID, redis.SetArgs{Mode: "NX", Get: true, TTL: msgstore.TTL}).Result()
		if err != nil && !errors.Is(err, redis.Nil) {
			return Result{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
		}
		if prev != "" {
			return p.duplicate(ctx, prev)
		}
	}
	// fail undoes everything done so far, so the customer can safely retry.
	var held []domain.Charge
	fail := func(err error) (Result, error) {
		bg, c := context.WithTimeout(context.Background(), time.Second)
		defer c()
		for _, h := range held {
			_ = p.Wallets.Release(bg, h.AccountID, h.HoldID)
		}
		_ = p.Store.Delete(bg, msgID)
		if idemKey != "" {
			_ = p.Redis.Del(bg, idemKey).Err()
		}
		return Result{}, err
	}

	chain := []string{req.AccountID}
	if p.Chain != nil {
		if chain, err = p.Chain.Chain(ctx, req.AccountID); err != nil {
			if errors.Is(err, hierarchy.ErrInactive) {
				return fail(err)
			}
			return fail(fmt.Errorf("%w: %v", ErrUnavailable, err))
		}
	}
	// Each paying account is priced by its own rate card: the sender at the
	// price its reseller set, each reseller at the price charged to it.
	charges := make([]domain.Charge, 0, len(chain))
	for i, acct := range chain {
		unit, err := p.Pricer.Price(ctx, acct, dest.MCC, dest.MNC, "*")
		if err != nil {
			switch {
			case errors.Is(err, pricing.ErrNoPrice) && i == 0:
				return fail(err)
			case errors.Is(err, pricing.ErrNoPrice):
				return fail(ErrResellerNoPrice)
			}
			return fail(fmt.Errorf("%w: %v", ErrUnavailable, err))
		}
		charges = append(charges, domain.Charge{AccountID: acct, HoldID: msgID, Amount: unit.Mul(decimal.NewFromInt(int64(segments)))})
	}
	price := charges[0].Amount

	m := &domain.Message{
		ID: msgID, AccountID: req.AccountID, ClientRef: req.ClientRef, Channel: req.Channel,
		Sender: req.Sender, Destination: dest.E164, MCC: dest.MCC, MNC: dest.MNC,
		Text: req.Text, UDH: req.UDH, Segments: segments, DLT: req.DLT,
		RegisteredDelivery: req.RegisteredDelivery, Price: price, Charges: charges, AcceptedAt: time.Now().UTC(),
	}

	for i, ch := range charges {
		if !ch.Amount.IsPositive() {
			continue
		}
		if err := p.Wallets.Hold(ctx, ch.AccountID, ch.HoldID, wallet.FromDecimal(ch.Amount)); err != nil {
			switch {
			case errors.Is(err, wallet.ErrInsufficient) && i == 0:
				return fail(err)
			case errors.Is(err, wallet.ErrInsufficient):
				return fail(ErrResellerBalance)
			}
			return fail(fmt.Errorf("%w: %v", ErrUnavailable, err))
		}
		held = append(held, ch)
		if i == 0 {
			m.HoldID = ch.HoldID
		}
	}

	if err := p.Store.Create(ctx, m); err != nil {
		return fail(fmt.Errorf("%w: %v", ErrUnavailable, err))
	}
	rec, err := kafka.Record(kafka.TopicSubmit, msgID, m)
	if err == nil {
		err = kafka.ProduceJSON(ctx, p.Kafka, rec)
	}
	if err != nil {
		return fail(fmt.Errorf("%w: %v", ErrUnavailable, err))
	}
	return Result{MessageID: msgID, Segments: segments, Price: price}, nil
}

func (p *Pipeline) duplicate(ctx context.Context, msgID string) (Result, error) {
	st, err := p.Store.Get(ctx, msgID)
	if err != nil {
		// The first attempt failed after claiming the key, or the record expired.
		return Result{MessageID: msgID, Duplicate: true}, nil
	}
	price, _ := decimal.NewFromString(st.Price)
	return Result{MessageID: msgID, Segments: st.Segments, Price: price, Duplicate: true}, nil
}
