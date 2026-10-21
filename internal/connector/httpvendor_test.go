package connector

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jpamaze/amazeA2pSMS/internal/domain"
)

func profileJSON(t *testing.T, base string, enc string) HTTPProfile {
	t.Helper()
	p, err := ParseHTTPProfile(base,
		[]byte(`{"type":"bearer"}`),
		[]byte(`{"path":"/send","encoding":"`+enc+`","params":{"to":"{{to_national}}","from":"{{sender}}","msg":"{{text}}","entity":"{{pe_id}}","tpl":"{{template_id}}","uni":"{{unicode}}","ref":"{{id}}"}}`),
		[]byte(`{"success_path":"status","success_values":["success"],"id_path":"data.0.msgid","error_path":"code"}`),
		[]byte(`{"id_param":"msgid","status_param":"status","error_param":"err","delivered_values":["DELIVERED"],"expired_values":["EXPIRED"],"intermediate_values":["SENT"]}`))
	if err != nil {
		t.Fatal(err)
	}
	p.Secret, p.DLRToken, p.TPS = "k3y", "tok", 1000
	return p
}

func TestHTTPSendJSON(t *testing.T) {
	var got map[string]string
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"status":"success","data":[{"msgid":"V-1"}]}`))
	}))
	defer srv.Close()

	tr := NewHTTPTransport(profileJSON(t, srv.URL, "json"))
	m := &domain.Message{ID: "m1", Destination: "919812345678", Sender: "AMZTST", Text: "नमस्ते", Segments: 1,
		DLT: domain.DLTFields{PEID: "PE", TemplateID: "TPL"}}
	ids, err := tr.Send(context.Background(), m)
	if err != nil || len(ids) != 1 || ids[0] != "V-1" {
		t.Fatalf("send: %v %v", ids, err)
	}
	if auth != "Bearer k3y" || got["to"] != "9812345678" || got["msg"] != "नमस्ते" || got["uni"] != "1" ||
		got["entity"] != "PE" || got["tpl"] != "TPL" || got["ref"] != "m1" {
		t.Fatalf("request: auth %q body %v", auth, got)
	}
}

func TestHTTPErrorsAndRetry(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("to") {
		case "9800000001": // refused by the vendor
			_, _ = w.Write([]byte(`{"status":"failed","code":"dnd"}`))
		case "9800000002": // overloaded twice, then fine
			if calls.Add(1) <= 2 {
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			_, _ = w.Write([]byte(`{"status":"success","data":[{"msgid":"V-2"}]}`))
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer srv.Close()
	tr := NewHTTPTransport(profileJSON(t, srv.URL, "query"))

	_, err := tr.Send(context.Background(), &domain.Message{Destination: "919800000001", Text: "x"})
	var se *SendError
	if !errors.As(err, &se) || se.Code != "VENDOR_DND" || se.Temporary {
		t.Fatalf("vendor refusal: %v", err)
	}
	ids, err := tr.Send(context.Background(), &domain.Message{Destination: "919800000002", Text: "x"})
	if err != nil || ids[0] != "V-2" || calls.Load() != 3 {
		t.Fatalf("429 must be retried: %v %v calls=%d", ids, err, calls.Load())
	}
	_, err = tr.Send(context.Background(), &domain.Message{Destination: "919800000003", Text: "x"})
	if !errors.As(err, &se) || se.Code != "HTTP_400" {
		t.Fatalf("4xx is permanent: %v", err)
	}
}

func TestDLRHandler(t *testing.T) {
	p := profileJSON(t, "http://unused", "json")
	var journaled []domain.DLR
	fail := false
	h := DLRHandler(p, func(_ context.Context, d domain.DLR) error {
		if fail {
			return errors.New("kafka down")
		}
		journaled = append(journaled, d)
		return nil
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	do := func(method, target, body, ct string) int {
		r := httptest.NewRequest(method, target, strings.NewReader(body))
		if ct != "" {
			r.Header.Set("Content-Type", ct)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if c := do("GET", "/dlr/x?token=bad&msgid=1&status=DELIVERED", "", ""); c != 403 {
		t.Fatalf("bad token: %d", c)
	}
	if c := do("GET", "/dlr/x?token=tok&msgid=V-1&status=DELIVERED", "", ""); c != 200 {
		t.Fatalf("GET: %d", c)
	}
	if c := do("POST", "/dlr/x?token=tok", "msgid=V-2&status=UNDELIVERED&err=34", "application/x-www-form-urlencoded"); c != 200 {
		t.Fatalf("form: %d", c)
	}
	if c := do("POST", "/dlr/x?token=tok", `{"msgid":"V-3","status":"EXPIRED"}`, "application/json"); c != 200 {
		t.Fatalf("json: %d", c)
	}
	if c := do("GET", "/dlr/x?token=tok&msgid=V-4&status=SENT", "", ""); c != 200 {
		t.Fatalf("intermediate: %d", c)
	}
	fail = true
	if c := do("GET", "/dlr/x?token=tok&msgid=V-5&status=DELIVERED", "", ""); c != 503 {
		t.Fatalf("journal failure must not be acknowledged: %d", c)
	}
	if len(journaled) != 3 || journaled[0].Stat != "DELIVRD" || journaled[1].Stat != "UNDELIV" ||
		journaled[1].ErrorCode != "34" || journaled[2].Stat != "EXPIRED" {
		t.Fatalf("journaled: %+v", journaled)
	}
}
