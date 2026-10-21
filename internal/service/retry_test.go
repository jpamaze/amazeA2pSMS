package service

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

func TestRetryRidesOutAFailover(t *testing.T) {
	d := &Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx := context.Background()
	n := 0
	err := retry(ctx, d, "test", func() error {
		n++
		if n < 3 {
			return errors.New("terminating connection due to unexpected postmaster exit")
		}
		return nil
	})
	if err != nil || n != 3 {
		t.Fatalf("retry: %v after %d attempts", err, n)
	}

	old := RetryFor
	RetryFor = 600 * time.Millisecond
	defer func() { RetryFor = old }()
	start := time.Now()
	if err := retry(ctx, d, "test", func() error { return errors.New("down for good") }); err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("gives up after RetryFor: %v in %s", err, time.Since(start))
	}

	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := retry(cctx, d, "test", func() error { return errors.New("x") }); err == nil {
		t.Fatal("stops when the service stops")
	}
}
