package records

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/twmb/franz-go/pkg/kgo"
)

func TestClickHouseFailsOverBetweenReplicas(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	downURL := down.URL
	down.Close() // connection refused
	var roHits, okHits atomic.Int32
	readOnly := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		roHits.Add(1)
		w.WriteHeader(500)
		_, _ = w.Write([]byte("Code: 242. DB::Exception: Table is in readonly mode. (TABLE_IS_READ_ONLY)"))
	}))
	defer readOnly.Close()
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		okHits.Add(1)
		if strings.Contains(r.URL.Query().Get("query"), "bad") {
			w.WriteHeader(500)
			_, _ = w.Write([]byte("Code: 62. DB::Exception: Syntax error. (SYNTAX_ERROR)"))
			return
		}
		_, _ = w.Write([]byte("1\n"))
	}))
	defer ok.Close()

	ch := &ClickHouse{URL: downURL + "," + readOnly.URL + ", " + ok.URL}
	ctx := context.Background()
	rows, err := ch.Query(ctx, "SELECT 1")
	if err != nil || len(rows) != 1 || rows[0][0] != "1" {
		t.Fatalf("query: %v %v", rows, err)
	}
	// The client stays with the replica that answered.
	if _, err := ch.Query(ctx, "SELECT 1"); err != nil || roHits.Load() != 1 || okHits.Load() != 2 {
		t.Fatalf("sticky: %v ro=%d ok=%d", err, roHits.Load(), okHits.Load())
	}
	// A query error is the query's fault: no other replica is tried.
	if _, err := ch.Query(ctx, "SELECT bad"); err == nil || !strings.Contains(err.Error(), "SYNTAX_ERROR") || roHits.Load() != 1 {
		t.Fatalf("query error: %v ro=%d", err, roHits.Load())
	}
	// Every replica down: the last error is returned.
	none := &ClickHouse{URL: downURL + "," + readOnly.URL}
	if _, err := none.Query(ctx, "SELECT 1"); err == nil || !strings.Contains(err.Error(), "TABLE_IS_READ_ONLY") {
		t.Fatalf("all down: %v", err)
	}
}

func TestReplicatedEngine(t *testing.T) {
	for in, want := range map[string]string{
		"ENGINE = ReplacingMergeTree(updated_at)": "ENGINE = ReplicatedReplacingMergeTree(updated_at)",
		"ENGINE=MergeTree()":                      "ENGINE = ReplicatedMergeTree()",
		"ENGINE = ReplicatedMergeTree()":          "ENGINE = ReplicatedMergeTree()",
	} {
		if got := replicatedEngine.ReplaceAllString(in, "ENGINE = Replicated${1}MergeTree("); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

func TestWriterSkipsRowsWithoutIDs(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
	}))
	defer srv.Close()
	w := &Writer{CH: &ClickHouse{URL: srv.URL}}
	good := Row{MessageID: "0194f1a2-0000-7000-8000-000000000001", AccountID: "0194f1a2-0000-7000-8000-000000000002", Status: "delivered"}
	partial := Row{MessageID: "0194f1a2-0000-7000-8000-000000000003", Status: "delivered"} // record rebuilt from partial state
	var recs []*kgo.Record
	for _, r := range []Row{good, partial} {
		v, _ := json.Marshal(r)
		recs = append(recs, &kgo.Record{Topic: Topic, Value: v})
	}
	if err := w.Process(context.Background(), recs); err != nil {
		t.Fatal(err)
	}
	if strings.Count(body, "\n") != 1 || !strings.Contains(body, good.MessageID) {
		t.Fatalf("inserted: %q", body)
	}
}
