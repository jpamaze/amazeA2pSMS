// Package records turns message events into rows of the ClickHouse
// message_records table and writes them in batches over the ClickHouse HTTP
// interface.
//
// Every row is a full snapshot; the ReplacingMergeTree keeps the one with the
// latest updated_at per (account_id, accepted_at, message_id), so replays and
// duplicates collapse.
package records

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/jpamaze/amazeA2pSMS/internal/domain"
	"github.com/jpamaze/amazeA2pSMS/internal/msgstore"
	"github.com/jpamaze/amazeA2pSMS/internal/platform/kafka"
)

// Topic carries final-state snapshots from the DLR processor.
const Topic = "sms.records"

// Row is one message_records row (JSONEachRow field names = column names).
type Row struct {
	MessageID   string  `json:"message_id"`
	AccountID   string  `json:"account_id"`
	Channel     string  `json:"channel"`
	ClientRef   string  `json:"client_ref"`
	Sender      string  `json:"sender"`
	Destination string  `json:"destination"`
	MCC         string  `json:"mcc"`
	MNC         string  `json:"mnc"`
	Segments    int     `json:"segments"`
	PEID        string  `json:"pe_id"`
	TemplateID  string  `json:"template_id"`
	VendorID    string  `json:"vendor_id"`
	VendorMsgID string  `json:"vendor_msg_id"`
	Status      string  `json:"status"`
	ErrorCode   string  `json:"error_code"`
	Price       string  `json:"price"`
	Cost        string  `json:"cost"`
	AcceptedAt  string  `json:"accepted_at"`
	DoneAt      *string `json:"done_at"`
	UpdatedAt   string  `json:"updated_at"`
}

const tsLayout = "2006-01-02 15:04:05.000"

func ts(t time.Time) string { return t.UTC().Format(tsLayout) }

func orZero(s string) string {
	if s == "" {
		return "0"
	}
	return s
}

// FromMessage is the row written when a message is accepted.
func FromMessage(m *domain.Message) Row {
	return Row{
		MessageID: m.ID, AccountID: m.AccountID, Channel: string(m.Channel), ClientRef: m.ClientRef,
		Sender: m.Sender, Destination: m.Destination, MCC: m.MCC, MNC: m.MNC, Segments: m.Segments,
		PEID: m.DLT.PEID, TemplateID: m.DLT.TemplateID, Status: string(domain.StatusAccepted),
		Price: orZero(m.Price.String()), Cost: "0",
		AcceptedAt: ts(m.AcceptedAt), UpdatedAt: ts(m.AcceptedAt),
	}
}

// FromState is the row written when a message reaches its final status.
func FromState(st msgstore.State) Row {
	r := Row{
		MessageID: st.ID, AccountID: st.AccountID, Channel: string(st.Channel), ClientRef: st.ClientRef,
		Sender: st.Sender, Destination: st.Destination, MCC: st.MCC, MNC: st.MNC, Segments: st.Segments,
		PEID: st.PEID, TemplateID: st.TemplateID, VendorID: st.VendorID, VendorMsgID: st.VendorMsgID,
		Status: string(st.Status), ErrorCode: st.ErrorCode, Price: orZero(st.Price), Cost: orZero(st.Cost),
		AcceptedAt: ts(st.AcceptedAt), UpdatedAt: ts(time.Now()),
	}
	if !st.DoneAt.IsZero() {
		d := ts(st.DoneAt)
		r.DoneAt = &d
		if st.DoneAt.After(st.AcceptedAt) {
			r.UpdatedAt = ts(st.DoneAt)
		}
	}
	return r
}

// Publish queues a final-state snapshot.
func Publish(ctx context.Context, cl *kgo.Client, st msgstore.State) error {
	rec, err := kafka.Record(Topic, st.ID, FromState(st))
	if err != nil {
		return err
	}
	return kafka.ProduceJSON(ctx, cl, rec)
}

// ClickHouse is a minimal client for the HTTP interface.
type ClickHouse struct {
	URL      string // e.g. http://localhost:8123
	Database string
	User     string
	Password string
	HTTP     *http.Client
}

// Exec runs one statement, with data appended in the body when given.
func (c *ClickHouse) Exec(ctx context.Context, query string, data []byte) error {
	u, err := url.Parse(c.URL)
	if err != nil {
		return err
	}
	q := u.Query()
	q.Set("query", query)
	if c.Database != "" {
		q.Set("database", c.Database)
	}
	q.Set("date_time_input_format", "best_effort")
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(data))
	if err != nil {
		return err
	}
	if c.User != "" {
		req.SetBasicAuth(c.User, c.Password)
	}
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("clickhouse: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// EnsureSchema applies every *.sql file in fsys/dir (all are idempotent).
func (c *ClickHouse) EnsureSchema(ctx context.Context, fsys fs.FS, dir string) error {
	names, err := fs.Glob(fsys, dir+"/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	for _, n := range names {
		b, err := fs.ReadFile(fsys, n)
		if err != nil {
			return err
		}
		if err := c.Exec(ctx, string(b), nil); err != nil {
			return fmt.Errorf("%s: %w", n, err)
		}
	}
	return nil
}

// Writer converts sms.submit and sms.records records to rows and inserts them.
type Writer struct {
	CH *ClickHouse
}

// Process inserts one batch. An insert failure returns an error so the batch
// is retried (rows are idempotent).
func (w *Writer) Process(ctx context.Context, recs []*kgo.Record) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	n := 0
	for _, rec := range recs {
		var row Row
		switch rec.Topic {
		case kafka.TopicSubmit:
			var m domain.Message
			if err := json.Unmarshal(rec.Value, &m); err != nil {
				continue
			}
			row = FromMessage(&m)
		case Topic:
			if err := json.Unmarshal(rec.Value, &row); err != nil {
				continue
			}
		default:
			continue
		}
		if err := enc.Encode(row); err != nil {
			return err
		}
		n++
	}
	if n == 0 {
		return nil
	}
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		if err = w.CH.Exec(ctx, "INSERT INTO message_records FORMAT JSONEachRow", buf.Bytes()); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt+1) * time.Second):
		}
	}
	return err
}
