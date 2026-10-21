// Package service is the shared main for every platform service: it loads
// config, opens the dependencies a service asks for, serves the ops
// endpoints (/healthz, /readyz, /metrics) and shuts down cleanly on SIGTERM.
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/jpamaze/amazeA2pSMS/internal/platform/config"
	"github.com/jpamaze/amazeA2pSMS/internal/platform/health"
	"github.com/jpamaze/amazeA2pSMS/internal/platform/kafka"
	applog "github.com/jpamaze/amazeA2pSMS/internal/platform/log"
	"github.com/jpamaze/amazeA2pSMS/internal/platform/metrics"
	"github.com/jpamaze/amazeA2pSMS/internal/platform/pg"
	"github.com/jpamaze/amazeA2pSMS/internal/platform/redisx"
)

// Needs lists the dependencies a service uses. Each one is opened and added
// to readiness.
type Needs struct {
	Postgres bool
	Redis    bool
	Kafka    bool
}

// Deps are the opened dependencies handed to a service's main function.
type Deps struct {
	Cfg    config.Common
	Log    *slog.Logger
	Health *health.Registry
	PG     *pgxpool.Pool         // nil unless Needs.Postgres
	Redis  redis.UniversalClient // nil unless Needs.Redis
	Kafka  *kgo.Client           // producer; nil unless Needs.Kafka
}

// MainFunc runs the service until ctx is cancelled.
type MainFunc func(ctx context.Context, d *Deps) error

// Run is the entry point used by every cmd/*/main.go.
func Run(name string, needs Needs, main MainFunc) {
	var cfg config.Common
	if err := config.Load(&cfg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if cfg.ServiceName == "" {
		cfg.ServiceName = name
	}
	logger := applog.New(cfg.ServiceName, cfg.LogLevel)

	if err := run(cfg, logger, needs, main); err != nil {
		logger.Error("service stopped with error", "err", err)
		os.Exit(1)
	}
	logger.Info("service stopped")
}

func run(cfg config.Common, logger *slog.Logger, needs Needs, main MainFunc) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	d := &Deps{Cfg: cfg, Log: logger, Health: health.NewRegistry()}

	if needs.Postgres {
		pool, err := pg.New(ctx, cfg.PostgresURL)
		if err != nil {
			return err
		}
		defer pool.Close()
		d.PG = pool
		d.Health.Add("postgres", pg.Ping(pool))
	}
	if needs.Redis {
		rc := redisx.New(cfg.RedisAddrs)
		defer rc.Close()
		d.Redis = rc
		d.Health.Add("redis", redisx.Ping(rc))
	}
	if needs.Kafka {
		kc, err := kafka.NewProducer(cfg.KafkaBrokers)
		if err != nil {
			return err
		}
		defer kc.Close()
		d.Kafka = kc
		d.Health.Add("kafka", kafka.Ping(kc))
	}

	ops := opsServer(cfg.OpsAddr, d.Health)
	opsErr := make(chan error, 1)
	go func() {
		logger.Info("ops server listening", "addr", cfg.OpsAddr)
		if err := ops.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			opsErr <- err
		}
	}()

	mainErr := make(chan error, 1)
	go func() { mainErr <- main(ctx, d) }()

	var err error
	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received")
		err = <-mainErr
	case err = <-mainErr:
	case err = <-opsErr:
	}
	stop()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = ops.Shutdown(shutdownCtx)
	return err
}

func opsServer(addr string, h *health.Registry) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("GET /healthz", health.LiveHandler())
	mux.Handle("GET /readyz", h.ReadyHandler())
	mux.Handle("GET /metrics", metrics.Handler())
	if os.Getenv("PPROF") == "true" {
		// Profiling for load tests; keep the ops port off public networks.
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	}
	return &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
}

// ServeHTTP runs srv until ctx is cancelled, then shuts it down gracefully.
func ServeHTTP(ctx context.Context, srv *http.Server) error {
	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}
