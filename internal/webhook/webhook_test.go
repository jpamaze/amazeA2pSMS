package webhook

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
)

type fixedTarget struct{ t *Target }

func (f fixedTarget) Get(context.Context, string) (*Target, error) { return f.t, nil }

func record(t *testing.T, env envelope) *kgo.Record {
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return &kgo.Record{Topic: TopicEvents, Value: b}
}

func TestSignedDeliveryAndRetry(t *testing.T) {
	var calls atomic.Int32
	var gotSig, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody, gotSig = string(b), r.Header.Get("X-Amaze-Signature")
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusBadGateway) // first attempt fails
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	d := &Dispatcher{
		Targets: fixedTarget{&Target{URL: srv.URL, Secret: "whsec_test"}},
		HTTP:    NewHTTPClient(true),
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	env := envelope{Event: Event{Event: "message.status", ID: "m1", Status: "delivered", To: "919812345678"}, AccountID: "a1"}

	next, err := d.handle(context.Background(), record(t, env))
	if err != nil || next == nil || next.Topic != RetryTopic(0) {
		t.Fatalf("first failure must go to retry tier 1: %+v %v", next, err)
	}
	var retry envelope
	_ = json.Unmarshal(next.Value, &retry)
	if retry.Attempt != 1 || retry.NextAt.Before(time.Now().Add(25*time.Second)) || !strings.Contains(retry.LastError, "502") {
		t.Fatalf("retry envelope: %+v", retry)
	}

	retry.NextAt = time.Now() // pretend the delay passed
	next, err = d.handle(context.Background(), record(t, retry))
	if err != nil || next != nil {
		t.Fatalf("second attempt should succeed: %+v %v", next, err)
	}

	// The receiver can verify the signature from the secret, timestamp and body.
	tsi, err := strconv.ParseInt(strings.TrimPrefix(strings.Split(gotSig, ",")[0], "t="), 10, 64)
	if err != nil {
		t.Fatalf("signature header: %q", gotSig)
	}
	if want := Sign("whsec_test", time.Unix(tsi, 0), []byte(gotBody)); want != gotSig {
		t.Fatalf("signature mismatch: %s vs %s", gotSig, want)
	}
	if !strings.Contains(gotBody, `"status":"delivered"`) || strings.Contains(gotBody, "account") {
		t.Fatalf("body: %s", gotBody)
	}
}

func TestDeadLetterAfterLastTier(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }))
	defer srv.Close()
	d := &Dispatcher{Targets: fixedTarget{&Target{URL: srv.URL, Secret: "s"}}, HTTP: NewHTTPClient(true),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	env := envelope{Event: Event{ID: "m2"}, AccountID: "a", Attempt: len(RetryDelays)}
	next, err := d.handle(context.Background(), record(t, env))
	if err != nil || next == nil || next.Topic != TopicDead {
		t.Fatalf("want dead letter: %+v %v", next, err)
	}
}

func TestNoTargetIsSkipped(t *testing.T) {
	d := &Dispatcher{Targets: fixedTarget{nil}, HTTP: NewHTTPClient(true), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	next, err := d.handle(context.Background(), record(t, envelope{Event: Event{ID: "m3"}, AccountID: "a"}))
	if err != nil || next != nil {
		t.Fatalf("no webhook configured must be a no-op: %+v %v", next, err)
	}
}

func TestPrivateAddressesBlocked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()
	_, err := NewHTTPClient(false).Get(srv.URL) // 127.0.0.1
	if err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("loopback webhook must be refused, got %v", err)
	}
}

func TestPublicAddr(t *testing.T) {
	for addr, want := range map[string]bool{
		"8.8.8.8": true, "2606:4700::1111": true,
		"127.0.0.1": false, "10.1.2.3": false, "172.16.0.1": false, "192.168.1.1": false,
		"169.254.169.254": false, "100.100.100.200": false, "0.0.0.0": false, "198.18.0.1": false,
		"::1": false, "fe80::1": false, "fd00::1": false, "::ffff:10.0.0.1": false,
		"64:ff9b::a00:1": false, "2002:a00:1::": false, "240.0.0.1": false,
	} {
		if got := PublicAddr(netip.MustParseAddr(addr)); got != want {
			t.Errorf("%s: public=%v, want %v", addr, got, want)
		}
	}
}
