// Package webhook delivers final message status to HTTP customers.
//
// The DLR processor publishes one Event per message to sms.webhooks. The
// dispatcher POSTs it to the account's webhook URL, signed with the account
// secret. Failures move to retry topics with growing delays; each topic has
// one fixed delay so waiting on one record never holds up an earlier one.
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/jpamaze/amazeA2pSMS/internal/platform/metrics"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/jpamaze/amazeA2pSMS/internal/platform/kafka"
	"github.com/jpamaze/amazeA2pSMS/internal/platform/secrets"
)

// Topic names.
const (
	TopicEvents = "sms.webhooks"
	TopicDead   = "sms.webhooks.dead"
)

// RetryDelays is the wait before each retry tier.
var RetryDelays = []time.Duration{30 * time.Second, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour}

// RetryTopic is the topic for retry tier i (0-based).
func RetryTopic(i int) string { return fmt.Sprintf("sms.webhooks.retry.%d", i+1) }

// Topics lists every webhook topic.
func Topics() []string {
	out := []string{TopicEvents, TopicDead}
	for i := range RetryDelays {
		out = append(out, RetryTopic(i))
	}
	return out
}

// Event is the payload POSTed to the customer (and carried on Kafka).
type Event struct {
	Event       string `json:"event"` // message.status
	ID          string `json:"id"`
	AccountID   string `json:"-"`
	Status      string `json:"status"`
	ErrorCode   string `json:"error_code,omitempty"`
	To          string `json:"to"`
	Sender      string `json:"sender"`
	Segments    int    `json:"segments"`
	Cost        string `json:"cost"`
	ClientRef   string `json:"client_ref,omitempty"`
	AcceptedAt  string `json:"accepted_at"`
	CompletedAt string `json:"completed_at,omitempty"`
}

// envelope is the Kafka record: the event plus delivery bookkeeping.
type envelope struct {
	Event
	AccountID string    `json:"account_id"`
	Attempt   int       `json:"attempt"`
	NextAt    time.Time `json:"next_at,omitzero"`
	LastError string    `json:"last_error,omitempty"`
}

// NewRecord builds the Kafka record that queues ev for delivery.
func NewRecord(ev Event) (*kgo.Record, error) {
	ev.Event = "message.status"
	return kafka.Record(TopicEvents, ev.ID, envelope{Event: ev, AccountID: ev.AccountID})
}

// Sign returns the X-Amaze-Signature header value: t=<unix>,v1=<hex HMAC-SHA256 of "<t>.<body>">.
func Sign(secret string, t time.Time, body []byte) string {
	ts := strconv.FormatInt(t.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write(body)
	return "t=" + ts + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

// Target is an account's webhook endpoint.
type Target struct {
	URL    string
	Secret string
}

// Targets loads and caches webhook endpoints per account.
type Targets struct {
	PG  *pgxpool.Pool
	Box *secrets.Box
	TTL time.Duration

	mu    sync.Mutex
	cache map[string]cachedTarget
}

type cachedTarget struct {
	t       *Target
	expires time.Time
}

// Get returns the account's endpoint, or nil when none is configured.
func (ts *Targets) Get(ctx context.Context, accountID string) (*Target, error) {
	ts.mu.Lock()
	if ts.cache == nil {
		ts.cache = map[string]cachedTarget{}
	}
	c, ok := ts.cache[accountID]
	ts.mu.Unlock()
	if ok && time.Now().Before(c.expires) {
		return c.t, nil
	}
	var u *string
	var enc []byte
	err := ts.PG.QueryRow(ctx, `SELECT webhook_url, webhook_secret_enc FROM accounts WHERE id = $1`, accountID).Scan(&u, &enc)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	var t *Target
	if u != nil && *u != "" && len(enc) > 0 {
		secret, err := ts.Box.Open(enc)
		if err != nil {
			return nil, err
		}
		t = &Target{URL: *u, Secret: string(secret)}
	}
	ts.mu.Lock()
	ts.cache[accountID] = cachedTarget{t: t, expires: time.Now().Add(ts.TTL)}
	ts.mu.Unlock()
	return t, nil
}

// TargetSource returns an account's webhook endpoint.
type TargetSource interface {
	Get(ctx context.Context, accountID string) (*Target, error)
}

// Dispatcher posts events and schedules retries.
type Dispatcher struct {
	Targets TargetSource
	Kafka   *kgo.Client
	HTTP    *http.Client
	Log     *slog.Logger
	Workers int
}

// NewHTTPClient returns a client with a 10 s timeout. Unless allowPrivate,
// it refuses to connect to loopback, private, link-local or unspecified
// addresses, so a customer cannot point a webhook at internal services.
func NewHTTPClient(allowPrivate bool) *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	if !allowPrivate {
		dialer.Control = func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip := net.ParseIP(host)
			if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
				ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
				return fmt.Errorf("webhook: refusing to connect to %s", host)
			}
			return nil
		}
	}
	tr := &http.Transport{
		DialContext:         dialer.DialContext,
		MaxIdleConnsPerHost: 32,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 5 * time.Second,
	}
	return &http.Client{
		Timeout:   10 * time.Second,
		Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse // never follow redirects
		},
	}
}

// Process delivers a batch from one topic. Records from a retry topic are
// held until their NextAt.
func (d *Dispatcher) Process(ctx context.Context, recs []*kgo.Record) error {
	workers := d.Workers
	if workers <= 0 {
		workers = 128
	}
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		out  []*kgo.Record
		ferr error
	)
	jobs := make(chan *kgo.Record)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for rec := range jobs {
				next, err := d.handle(ctx, rec)
				mu.Lock()
				if err != nil && ferr == nil {
					ferr = err
				}
				if next != nil {
					out = append(out, next)
				}
				mu.Unlock()
			}
		}()
	}
	for _, rec := range recs {
		select {
		case jobs <- rec:
		case <-ctx.Done():
		}
	}
	close(jobs)
	wg.Wait()
	if ferr != nil {
		return ferr
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return kafka.ProduceJSON(ctx, d.Kafka, out...)
}

// handle returns a record to publish (retry or dead letter), or an
// infrastructure error that should stop the batch.
func (d *Dispatcher) handle(ctx context.Context, rec *kgo.Record) (*kgo.Record, error) {
	var env envelope
	if err := json.Unmarshal(rec.Value, &env); err != nil {
		d.Log.Error("bad webhook record; skipping", "topic", rec.Topic, "offset", rec.Offset, "err", err)
		return nil, nil
	}
	if wait := time.Until(env.NextAt); wait > 0 {
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return nil, nil
		}
	}
	target, err := d.Targets.Get(ctx, env.AccountID)
	if err != nil {
		return nil, err
	}
	if target == nil {
		return nil, nil // the account has no webhook
	}

	err = d.post(ctx, target, env.Event)
	if err == nil {
		metrics.Webhooks.WithLabelValues("ok").Inc()
		metrics.CustomerReceipts.WithLabelValues("http").Inc()
		return nil, nil
	}
	if ctx.Err() != nil {
		return nil, nil
	}
	env.LastError = err.Error()
	if env.Attempt >= len(RetryDelays) {
		d.Log.Warn("webhook dropped after retries", "message", env.ID, "account", env.AccountID, "err", err)
		metrics.Webhooks.WithLabelValues("dead").Inc()
		return kafka.Record(TopicDead, env.ID, env)
	}
	metrics.Webhooks.WithLabelValues("retry").Inc()
	tier := env.Attempt
	env.Attempt++
	env.NextAt = time.Now().Add(RetryDelays[tier])
	return kafka.Record(RetryTopic(tier), env.ID, env)
}

func (d *Dispatcher) post(ctx context.Context, t *Target, ev Event) error {
	body, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Amaze-Webhooks/1")
	req.Header.Set("X-Amaze-Event", ev.Event)
	req.Header.Set("X-Amaze-Delivery", uuid.NewString())
	req.Header.Set("X-Amaze-Signature", Sign(t.Secret, time.Now(), body))
	resp, err := d.HTTP.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64*1024))
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("webhook: HTTP %d", resp.StatusCode)
	}
	return nil
}
