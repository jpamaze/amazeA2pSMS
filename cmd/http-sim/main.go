// Command http-sim is a fake HTTP SMS vendor for development and load tests.
// POST /send (JSON, Bearer auth) returns {"status":"success","data":[{"msgid":...}]}
// and, after a delay, calls the DLR URL with msgid and status.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"
)

func main() {
	addr := flag.String("addr", ":2777", "listen address")
	key := flag.String("key", "simkey", "expected Bearer token")
	dlrURL := flag.String("dlr-url", "", "delivery callback URL (msgid, status, err are appended)")
	delay := flag.Duration("dlr-delay", 200*time.Millisecond, "delay before the callback")
	failRate := flag.Float64("fail", 0.02, "fraction reported UNDELIVERED")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	var seq, sent, dlrs atomic.Int64
	client := &http.Client{Timeout: 10 * time.Second}

	callback := func(id string) {
		status, errc := "DELIVERED", "0"
		if rand.Float64() < *failRate {
			status, errc = "UNDELIVERED", "34"
		}
		u, err := url.Parse(*dlrURL)
		if err != nil {
			return
		}
		q := u.Query()
		q.Set("msgid", id)
		q.Set("status", status)
		q.Set("err", errc)
		u.RawQuery = q.Encode()
		for attempt := 0; attempt < 5; attempt++ {
			resp, err := client.Get(u.String())
			if err == nil {
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					dlrs.Add(1)
					return
				}
			}
			time.Sleep(time.Second << attempt)
		}
		log.Warn("callback gave up", "msgid", id)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /send", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+*key {
			http.Error(w, `{"status":"failed","code":"auth"}`, http.StatusUnauthorized)
			return
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["to"] == "" || body["msg"] == "" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"failed","code":"bad_request"}`))
			return
		}
		id := fmt.Sprintf("H%015d", seq.Add(1))
		sent.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"status":"success","data":[{"msgid":%q}]}`, id)
		if *dlrURL != "" {
			time.AfterFunc(*delay, func() { callback(id) })
		}
	})

	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()
		var last int64
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if n := sent.Load(); n != last {
					log.Info("http-sim stats", "sent", n, "tps", (n-last)/5, "dlrs", dlrs.Load())
					last = n
				}
			}
		}
	}()
	go func() { <-ctx.Done(); _ = srv.Shutdown(context.Background()) }()
	log.Info("http-sim listening", "addr", *addr, "dlr_url", *dlrURL)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Error("listen", "err", err)
		os.Exit(1)
	}
}
