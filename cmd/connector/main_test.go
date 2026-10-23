package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"
)

func TestDLRRoutes(t *testing.T) {
	r := &dlrRoutes{m: map[string]http.Handler{}}
	r.set("v1", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusAccepted) }))
	get := func(path string) int {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("POST", path, nil))
		return w.Code
	}
	if c := get("/dlr/v1?token=x"); c != http.StatusAccepted {
		t.Fatalf("running vendor: %d", c)
	}
	for _, p := range []string{"/dlr/v2", "/dlr/", "/other/v1", "/dlr/v1/extra"} {
		if c := get(p); c != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", p, c)
		}
	}
	r.set("v1", nil)
	if c := get("/dlr/v1"); c != http.StatusNotFound {
		t.Fatalf("stopped vendor: %d", c)
	}
}

func TestEnsureTopic(t *testing.T) {
	c, err := kfake.NewCluster(kfake.NumBrokers(1))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	cl, err := kgo.NewClient(kgo.SeedBrokers(c.ListenAddrs()...))
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	admin := kadm.NewClient(cl)
	ctx := context.Background()
	for i := 0; i < 2; i++ { // the second call finds it and is fine
		if err := ensureTopic(ctx, admin, "sms.route.new", 6, 1); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	td, err := admin.ListTopics(ctx, "sms.route.new")
	if err != nil {
		t.Fatal(err)
	}
	if got := len(td["sms.route.new"].Partitions); got != 6 {
		t.Fatalf("partitions: %d", got)
	}
}
