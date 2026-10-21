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
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/jpamaze/amazeA2pSMS/internal/domain"
	"github.com/jpamaze/amazeA2pSMS/internal/msgstore"
	"github.com/jpamaze/amazeA2pSMS/internal/platform/kafka"
	"github.com/jpamaze/amazeA2pSMS/internal/platform/metrics"
)

// Topic carries final-state snapshots from the DLR processor.
const Topic = "sms.records"

// Row is one message_records row (JSONEachRow field names = column names).
type Row struct {
	MessageID   string            `json:"message_id"`
	AccountID   string            `json:"account_id"`
	Channel     string            `json:"channel"`
	ClientRef   string            `json:"client_ref"`
	Sender      string            `json:"sender"`
	Destination string            `json:"destination"`
	MCC         string            `json:"mcc"`
	MNC         string            `json:"mnc"`
	Segments    int               `json:"segments"`
	PEID        string            `json:"pe_id"`
	TemplateID  string            `json:"template_id"`
	DLTCategory string            `json:"dlt_category"`
	VendorID    string            `json:"vendor_id"`
	VendorMsgID string            `json:"vendor_msg_id"`
	Status      string            `json:"status"`
	ErrorCode   string            `json:"error_code"`
	Price       string            `json:"price"`
	Cost        string            `json:"cost"`
	AcceptedAt  string            `json:"accepted_at"`
	DoneAt      *string           `json:"done_at"`
	UpdatedAt   string            `json:"updated_at"`
	Charges     map[string]string `json:"charges"` // account ID -> amount charged
}

const tsLayout = "2006-01-02 15:04:05.000"

// ist is the time zone of the DateTime64 columns: ClickHouse reads a
// timestamp string as wall-clock time in the column's zone.
var ist = time.FixedZone("IST", 5*3600+1800)

func ts(t time.Time) string { return t.In(ist).Format(tsLayout) }

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
		PEID: m.DLT.PEID, TemplateID: m.DLT.TemplateID, DLTCategory: m.DLT.Category, Status: string(domain.StatusAccepted),
		Price: orZero(m.Price.String()), Cost: "0",
		AcceptedAt: ts(m.AcceptedAt), UpdatedAt: ts(m.AcceptedAt),
		Charges: chargeMap(m.Charges),
	}
}

func chargeMap(cs []domain.Charge) map[string]string {
	m := make(map[string]string, len(cs))
	for _, c := range cs {
		m[c.AccountID] = c.Amount.String()
	}
	return m
}

// FromState is the row written when a message reaches its final status.
func FromState(st msgstore.State) Row {
	r := Row{
		MessageID: st.ID, AccountID: st.AccountID, Channel: string(st.Channel), ClientRef: st.ClientRef,
		Sender: st.Sender, Destination: st.Destination, MCC: st.MCC, MNC: st.MNC, Segments: st.Segments,
		PEID: st.PEID, TemplateID: st.TemplateID, DLTCategory: st.DLTCategory, VendorID: st.VendorID, VendorMsgID: st.VendorMsgID,
		Status: string(st.Status), ErrorCode: st.ErrorCode, Price: orZero(st.Price), Cost: orZero(st.Cost),
		AcceptedAt: ts(st.AcceptedAt), UpdatedAt: ts(time.Now()), Charges: st.Charges,
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

// NewRecord builds the Kafka record for a final-state snapshot.
func NewRecord(st msgstore.State) (*kgo.Record, error) {
	return kafka.Record(Topic, st.ID, FromState(st))
}

// ClickHouse is a minimal client for the HTTP interface.
type ClickHouse struct {
	// URL is one server, e.g. http://localhost:8123, or several replicas
	// separated by commas: a request that cannot reach a replica, or finds
	// it read-only (lost its Keeper quorum), goes to the next, and the
	// client stays with the one that answered.
	URL      string
	Database string
	User     string
	Password string
	HTTP     *http.Client

	preferred atomic.Int32
}

// Exec runs one statement, with data appended in the body when given.
func (c *ClickHouse) Exec(ctx context.Context, query string, data []byte) error {
	_, err := c.do(ctx, query, data)
	return err
}

// Query runs a SELECT and returns TabSeparated rows split into fields.
func (c *ClickHouse) Query(ctx context.Context, query string) ([][]string, error) {
	b, err := c.do(ctx, query+" FORMAT TabSeparated", nil)
	if err != nil {
		return nil, err
	}
	var out [][]string
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if line != "" {
			out = append(out, strings.Split(line, "\t"))
		}
	}
	return out, nil
}

func (c *ClickHouse) do(ctx context.Context, query string, data []byte) ([]byte, error) {
	urls := strings.Split(c.URL, ",")
	first := int(c.preferred.Load()) % len(urls)
	var err error
	for i := range urls {
		n := (first + i) % len(urls)
		var b []byte
		var retry bool
		b, retry, err = c.doOne(ctx, strings.TrimSpace(urls[n]), query, data)
		if err == nil {
			c.preferred.Store(int32(n))
			return b, nil
		}
		if !retry || ctx.Err() != nil {
			return nil, err
		}
	}
	return nil, err
}

// replicaDown are ClickHouse errors after which another replica may succeed.
var replicaDown = []string{"TABLE_IS_READ_ONLY", "NO_ZOOKEEPER", "KEEPER_EXCEPTION", "ALL_CONNECTION_TRIES_FAILED"}

// doOne sends one request; retry reports whether another replica may do better.
func (c *ClickHouse) doOne(ctx context.Context, base, query string, data []byte) ([]byte, bool, error) {
	u, err := url.Parse(base)
	if err != nil {
		return nil, false, err
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
		return nil, false, err
	}
	if c.User != "" {
		req.SetBasicAuth(c.User, c.Password)
	}
	client := c.HTTP
	if client == nil {
		client = defaultHTTP
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, true, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, true, err
	}
	if resp.StatusCode != http.StatusOK {
		if len(b) > 2048 {
			b = b[:2048]
		}
		msg := strings.TrimSpace(string(b))
		retry := resp.StatusCode == http.StatusBadGateway || resp.StatusCode == http.StatusServiceUnavailable
		for _, code := range replicaDown {
			retry = retry || strings.Contains(msg, code)
		}
		return nil, retry, fmt.Errorf("clickhouse %s: HTTP %d: %s", u.Host, resp.StatusCode, msg)
	}
	return b, false, nil
}

var defaultHTTP = &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
	DialContext:         (&net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	MaxIdleConnsPerHost: 16,
	IdleConnTimeout:     90 * time.Second,
}}

// EnsureSchema applies every *.sql file in fsys/dir (all are idempotent).
// In a Replicated database (the HA cluster, docs/phase5-ha.md) MergeTree
// engines become their Replicated forms, so every replica holds the data;
// DDL run on one replica reaches the others through the database.
func (c *ClickHouse) EnsureSchema(ctx context.Context, fsys fs.FS, dir string) error {
	names, err := fs.Glob(fsys, dir+"/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	rows, err := c.Query(ctx, "SELECT engine FROM system.databases WHERE name = currentDatabase()")
	if err != nil {
		return err
	}
	replicated := len(rows) == 1 && rows[0][0] == "Replicated"
	for _, n := range names {
		b, err := fs.ReadFile(fsys, n)
		if err != nil {
			return err
		}
		q := string(b)
		if replicated {
			q = replicatedEngine.ReplaceAllString(q, "ENGINE = Replicated${1}MergeTree(")
		}
		if err := c.Exec(ctx, q, nil); err != nil {
			return fmt.Errorf("%s: %w", n, err)
		}
	}
	return nil
}

// replicatedEngine matches a non-replicated MergeTree engine clause.
var replicatedEngine = regexp.MustCompile(`ENGINE\s*=\s*((?:Replacing|Summing|Aggregating|Collapsing|VersionedCollapsing)?)MergeTree\(`)

// Writer converts sms.submit and sms.records records to rows and inserts them.
type Writer struct {
	CH  *ClickHouse
	Log *slog.Logger // nil: discarded
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
		// A row without valid IDs (a record rebuilt from partial state)
		// would make ClickHouse refuse the whole batch, forever.
		if !validUUID(row.MessageID) || !validUUID(row.AccountID) {
			metrics.RecordsDropped.Inc()
			if w.Log != nil {
				w.Log.Warn("message record without valid IDs; not written", "topic", rec.Topic, "offset", rec.Offset,
					"message", row.MessageID, "account", row.AccountID)
			}
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

func validUUID(s string) bool {
	_, err := uuid.Parse(s)
	return err == nil
}
