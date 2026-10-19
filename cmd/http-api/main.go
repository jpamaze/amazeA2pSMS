// Command http-api is the customer REST edge: authenticate, rate-limit,
// hold balance, write to Kafka, answer 202. It never calls a vendor
// (blueprint rule 2).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/caarlos0/env/v11"

	"github.com/jpamaze/amazeA2pSMS/internal/service"
)

type apiConfig struct {
	Addr string `env:"HTTP_ADDR" envDefault:":8080"`
}

func main() {
	var ac apiConfig
	service.Run("http-api", service.Needs{Postgres: true, Redis: true, Kafka: true}, func(ctx context.Context, d *service.Deps) error {
		if err := env.Parse(&ac); err != nil {
			return fmt.Errorf("http-api config: %w", err)
		}
		mux := http.NewServeMux()
		// TODO(phase 1, api/openapi.yaml): API-key auth, per-account token bucket,
		// DLT field check, wallet.Hold, produce to kafka.TopicSubmit, 202.
		mux.HandleFunc("POST /v1/sms", notImplemented)
		mux.HandleFunc("GET /v1/sms/{id}", notImplemented)
		mux.HandleFunc("GET /v1/wallet", notImplemented)

		d.Log.Info("http api listening", "addr", ac.Addr)
		return service.ServeHTTP(ctx, &http.Server{
			Addr:              ac.Addr,
			Handler:           mux,
			ReadHeaderTimeout: 5 * time.Second,
		})
	})
}

func notImplemented(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotImplemented)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "not implemented yet"})
}
