// Command wallet-resync rebuilds the Redis copy of every wallet from
// PostgreSQL (balance, credit limit, held) and drops stale holds. Run it
// after a Redis failover or restore; it is safe while traffic flows (see
// internal/billing/resync). -dry-run only reports.
//
// It exits 0 when nothing differed (or everything was repaired), 2 when
// -dry-run found differences, and 1 on errors.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/jpamaze/amazeA2pSMS/internal/billing/resync"
	"github.com/jpamaze/amazeA2pSMS/internal/platform/config"
	"github.com/jpamaze/amazeA2pSMS/internal/platform/pg"
	"github.com/jpamaze/amazeA2pSMS/internal/platform/redisx"
)

func main() {
	dry := flag.Bool("dry-run", false, "report differences without changing Redis")
	stale := flag.Duration("stale-after", time.Hour, "drop holds older than this whose message is no longer waiting to be sent")
	timeout := flag.Duration("timeout", 10*time.Minute, "give up after this long")
	flag.Parse()

	var cfg config.Common
	if err := config.Load(&cfg); err != nil {
		fail(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	pool, err := pg.New(ctx, cfg.PostgresURL)
	if err != nil {
		fail(err)
	}
	defer pool.Close()
	rdb := redisx.New(cfg.Redis())
	defer rdb.Close()

	rep, err := resync.Run(ctx, pool, rdb, resync.Options{DryRun: *dry, StaleAfter: *stale})
	if err != nil {
		fail(err)
	}
	verb := "repaired"
	if *dry {
		verb = "would repair"
	}
	for _, d := range rep.Drifts {
		fmt.Printf("%s %s: balance %s -> %s, credit limit %s -> %s, held %s -> %s, stale holds %d (%s)\n", verb, d.AccountID,
			d.RedisBalance.Decimal().StringFixed(4), d.Balance.Decimal().StringFixed(4),
			d.RedisLimit.Decimal().StringFixed(4), d.Limit.Decimal().StringFixed(4),
			d.HeldBefore.Decimal().StringFixed(4), d.HeldAfter.Decimal().StringFixed(4),
			d.StaleHolds, d.StaleAmount.Decimal().StringFixed(4))
	}
	fmt.Printf("%d wallets checked, %d %s, in %s\n", rep.Wallets, len(rep.Drifts), map[bool]string{true: "differ", false: "repaired"}[*dry], rep.Took.Round(time.Millisecond))
	if *dry && len(rep.Drifts) > 0 {
		os.Exit(2)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "wallet-resync:", err)
	os.Exit(1)
}
