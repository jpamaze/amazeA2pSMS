// Command billing-check verifies, after a load test has finished and the
// pipeline has drained, that money and message records agree:
//
//  1. no balance holds are left open in Redis;
//  2. every wallet's live balance in Redis equals its PostgreSQL balance;
//  3. per account, ledger charges since -since equal the price of the
//     messages ClickHouse shows a vendor accepted;
//  4. no message accepted since -since is still without a final status.
//
// It exits 0 when every check passes and 2 otherwise.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/shopspring/decimal"

	"github.com/jpamaze/amazeA2pSMS/internal/billing/wallet"
	"github.com/jpamaze/amazeA2pSMS/internal/platform/config"
	"github.com/jpamaze/amazeA2pSMS/internal/platform/pg"
	"github.com/jpamaze/amazeA2pSMS/internal/platform/redisx"
	"github.com/jpamaze/amazeA2pSMS/internal/records"
)

func main() {
	since := flag.String("since", "", "start of the test window, RFC 3339 (required)")
	settle := flag.Duration("settle", 2*time.Minute, "how long to wait for holds and balances to converge")
	chURL := flag.String("clickhouse", envOr("CLICKHOUSE_URL", "http://localhost:8123"), "ClickHouse HTTP URL")
	chDB := flag.String("clickhouse-db", envOr("CLICKHOUSE_DB", "sms"), "ClickHouse database")
	chUser := flag.String("clickhouse-user", envOr("CLICKHOUSE_USER", "sms"), "ClickHouse user")
	chPass := flag.String("clickhouse-password", envOr("CLICKHOUSE_PASSWORD", "sms"), "ClickHouse password")
	flag.Parse()

	from, err := time.Parse(time.RFC3339, *since)
	if err != nil {
		fmt.Fprintln(os.Stderr, "-since must be RFC 3339, e.g. 2026-10-21T10:00:00Z")
		os.Exit(1)
	}
	var cfg config.Common
	if err := config.Load(&cfg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	ctx := context.Background()
	pool, err := pg.New(ctx, cfg.PostgresURL)
	if err != nil {
		fatal(err)
	}
	rdb := redisx.New(cfg.RedisAddrs)
	ch := &records.ClickHouse{URL: *chURL, Database: *chDB, User: *chUser, Password: *chPass}

	var failures []string
	check := func(name string, ok bool, detail string) {
		mark := "PASS"
		if !ok {
			mark = "FAIL"
			failures = append(failures, name)
		}
		fmt.Printf("%-44s %s  %s\n", name, mark, detail)
	}

	// 1 + 2: holds drain and Redis catches up with PostgreSQL (the rater
	// writes in batches, so allow time to converge).
	deadline := time.Now().Add(*settle)
	var openHolds int64
	var mismatches []string
	for {
		openHolds, mismatches, err = walletState(ctx, rdb, func(acct string) (decimal.Decimal, error) {
			var b decimal.Decimal
			err := pool.QueryRow(ctx, `SELECT balance FROM wallets WHERE account_id = $1`, acct).Scan(&b)
			return b, err
		})
		if err != nil {
			fatal(err)
		}
		if (openHolds == 0 && len(mismatches) == 0) || time.Now().After(deadline) {
			break
		}
		time.Sleep(3 * time.Second)
	}
	check("no open balance holds", openHolds == 0, fmt.Sprintf("%d open", openHolds))
	check("Redis balance = PostgreSQL balance", len(mismatches) == 0, strings.Join(first(mismatches, 5), "; "))

	// 3: ledger charges vs ClickHouse, per account.
	ledger := map[string]decimal.Decimal{}
	rows, err := pool.Query(ctx, `SELECT account_id::text, -sum(amount) FROM ledger_entries
		WHERE kind = 'charge' AND created_at >= $1 GROUP BY account_id`, from)
	if err != nil {
		fatal(err)
	}
	for rows.Next() {
		var a string
		var v decimal.Decimal
		if err := rows.Scan(&a, &v); err != nil {
			fatal(err)
		}
		ledger[a] = v
	}
	rows.Close()
	ts := from.UTC().Format("2006-01-02 15:04:05")
	chRows, err := ch.Query(ctx, fmt.Sprintf(`SELECT toString(account_id), toString(sum(price)), count()
		FROM message_records FINAL
		WHERE accepted_at >= '%s' AND vendor_id != '' AND status IN ('submitted','delivered','failed','expired')
		GROUP BY account_id`, ts))
	if err != nil {
		fatal(err)
	}
	var billDiff []string
	var charged int64
	seen := map[string]bool{}
	for _, r := range chRows {
		want, _ := decimal.NewFromString(r[1])
		var n int64
		fmt.Sscan(r[2], &n)
		charged += n
		seen[r[0]] = true
		if got := ledger[r[0]]; !got.Equal(want) {
			billDiff = append(billDiff, fmt.Sprintf("%s ledger %s vs records %s", r[0], got.StringFixed(4), want.StringFixed(4)))
		}
	}
	for a, v := range ledger {
		if !seen[a] && !v.IsZero() {
			billDiff = append(billDiff, fmt.Sprintf("%s ledger %s vs records 0", a, v.StringFixed(4)))
		}
	}
	check("ledger charges = ClickHouse charged messages", len(billDiff) == 0,
		fmt.Sprintf("%d charged messages; %s", charged, strings.Join(first(billDiff, 5), "; ")))

	// 4: nothing stuck.
	stuck, err := ch.Query(ctx, fmt.Sprintf(`SELECT status, count() FROM message_records FINAL
		WHERE accepted_at >= '%s' AND status IN ('accepted','submitted') GROUP BY status`, ts))
	if err != nil {
		fatal(err)
	}
	var stuckN int64
	var parts []string
	for _, r := range stuck {
		var n int64
		fmt.Sscan(r[1], &n)
		stuckN += n
		parts = append(parts, r[0]+"="+r[1])
	}
	check("every message has a final status", stuckN == 0, strings.Join(parts, " "))

	if len(failures) > 0 {
		fmt.Println("RESULT: FAIL")
		os.Exit(2)
	}
	fmt.Println("RESULT: PASS")
}

// walletState counts open holds and compares each Redis wallet with the database.
func walletState(ctx context.Context, rdb redis.UniversalClient, dbBalance func(string) (decimal.Decimal, error)) (int64, []string, error) {
	var holds int64
	var diffs []string
	var keys []string
	scan := func(ctx context.Context, c *redis.Client) error {
		iter := c.Scan(ctx, 0, "wallet:{*}", 1000).Iterator()
		for iter.Next(ctx) {
			keys = append(keys, iter.Val())
		}
		return iter.Err()
	}
	switch c := rdb.(type) {
	case *redis.ClusterClient:
		if err := c.ForEachMaster(ctx, scan); err != nil {
			return 0, nil, err
		}
	case *redis.Client:
		if err := scan(ctx, c); err != nil {
			return 0, nil, err
		}
	}
	for _, k := range keys {
		if strings.HasSuffix(k, ":holds") {
			n, err := rdb.HLen(ctx, k).Result()
			if err != nil {
				return 0, nil, err
			}
			holds += n
			continue
		}
		acct := strings.TrimSuffix(strings.TrimPrefix(k, "wallet:{"), "}")
		snap, err := wallet.New(rdb).Get(ctx, acct)
		if err != nil {
			return 0, nil, err
		}
		db, err := dbBalance(acct)
		if err != nil {
			continue // wallet in Redis for an account that no longer exists
		}
		if !snap.Balance.Decimal().Equal(db) {
			diffs = append(diffs, fmt.Sprintf("%s redis %s db %s", acct, snap.Balance.Decimal().StringFixed(4), db.StringFixed(4)))
		}
	}
	return holds, diffs, nil
}

func first(s []string, n int) []string {
	if len(s) > n {
		return append(s[:n:n], fmt.Sprintf("(+%d more)", len(s)-n))
	}
	return s
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "billing-check:", err)
	os.Exit(1)
}
