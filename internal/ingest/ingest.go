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

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/shopspring/decimal"
	"github.com/twmb/franz-go/pkg/kgo"

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
	Pricer  pricing.Pricer
	Wallets *wallet.Store
	Store   *msgstore.Store
	Redis   redis.UniversalClient
	Kafka   *kgo.Client
	Timeout time.Duration // bound for the whole accept (default 3s)
}

// Accept validates, prices, holds and enqueues one message.
func (p *Pipeline) Accept(ctx context.Context, req Request) (Result, error) {
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
	held := false
	var holdID string
	fail := func(err error) (Result, error) {
		bg, c := context.WithTimeout(context.Background(), time.Second)
		defer c()
		if held {
			_ = p.Wallets.Release(bg, req.AccountID, holdID)
		}
		_ = p.Store.Delete(bg, msgID)
		if idemKey != "" {
			_ = p.Redis.Del(bg, idemKey).Err()
		}
		return Result{}, err
	}

	unit, err := p.Pricer.Price(ctx, req.AccountID, dest.MCC, dest.MNC, "*")
	if err != nil {
		if errors.Is(err, pricing.ErrNoPrice) {
			return fail(err)
		}
		return fail(fmt.Errorf("%w: %v", ErrUnavailable, err))
	}
	price := unit.Mul(decimal.NewFromInt(int64(segments)))

	m := &domain.Message{
		ID: msgID, AccountID: req.AccountID, ClientRef: req.ClientRef, Channel: req.Channel,
		Sender: req.Sender, Destination: dest.E164, MCC: dest.MCC, MNC: dest.MNC,
		Text: req.Text, UDH: req.UDH, Segments: segments, DLT: req.DLT,
		RegisteredDelivery: req.RegisteredDelivery, Price: price, AcceptedAt: time.Now().UTC(),
	}

	if price.IsPositive() {
		holdID = msgID
		m.HoldID = holdID
		if err := p.Wallets.Hold(ctx, req.AccountID, holdID, wallet.FromDecimal(price)); err != nil {
			if errors.Is(err, wallet.ErrInsufficient) {
				return fail(err)
			}
			return fail(fmt.Errorf("%w: %v", ErrUnavailable, err))
		}
		held = true
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
