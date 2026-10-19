package credit

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/jpamaze/amazeA2pSMS/internal/platform/pg"
	"github.com/jpamaze/amazeA2pSMS/migrations"
)

// testPool resets POSTGRES_TEST_URL (default: local sms_test database),
// applies all migrations and skips when no database is reachable.
// The database is wiped: never point this at real data.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("POSTGRES_TEST_URL")
	if url == "" {
		url = "postgres://sms:sms@localhost:5432/sms_test?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err == nil {
		err = pool.Ping(ctx)
	}
	if err != nil {
		t.Skipf("postgres not reachable: %v", err)
	}
	t.Cleanup(pool.Close)

	if _, err := pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, err := pg.Migrate(ctx, pool, migrations.Postgres, "postgres", log); err != nil {
		t.Fatal(err)
	}
	return pool
}

func newAccount(t *testing.T, pool *pgxpool.Pool, parent *string, role, name string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO accounts (parent_id, role, name) VALUES ($1, $2, $3) RETURNING id`,
		parent, role, name).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func TestCreditLimitRules(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	svc := New(pool, nil)

	platform := newAccount(t, pool, nil, "platform", "Amaze")
	reseller := newAccount(t, pool, &platform, "reseller", "Reseller A")
	subA := newAccount(t, pool, &reseller, "customer", "Sub A")
	subB := newAccount(t, pool, &reseller, "customer", "Sub B")
	direct := newAccount(t, pool, &platform, "customer", "Direct")

	// Every new wallet starts with credit limit 0.
	var limit decimal.Decimal
	if err := pool.QueryRow(ctx, `SELECT credit_limit FROM wallets WHERE account_id = $1`, subA).Scan(&limit); err != nil {
		t.Fatal(err)
	}
	if !limit.IsZero() {
		t.Fatalf("default credit limit: want 0, got %s", limit)
	}

	// Reseller has Rs 1,000 balance and Rs 500 credit from the platform: Rs 1,500 to hand out.
	if _, err := pool.Exec(ctx, `UPDATE wallets SET balance = 1000 WHERE account_id = $1`, reseller); err != nil {
		t.Fatal(err)
	}
	must(t, svc.SetLimit(ctx, Change{AccountID: reseller, NewLimit: dec("500"),
		ActorAccountID: platform, ActorUser: "ops@amaze", Reason: "pilot"}))

	must(t, svc.SetLimit(ctx, Change{AccountID: subA, NewLimit: dec("1000"),
		ActorAccountID: reseller, ActorUser: "admin@reseller", Reason: "monthly"}))
	must(t, svc.SetLimit(ctx, Change{AccountID: subB, NewLimit: dec("500"),
		ActorAccountID: reseller, ActorUser: "admin@reseller", Reason: "monthly"}))

	// Rs 1,500 already given out; one more rupee is over.
	err := svc.SetLimit(ctx, Change{AccountID: subB, NewLimit: dec("501"),
		ActorAccountID: reseller, ActorUser: "admin@reseller", Reason: "more"})
	if !errors.Is(err, ErrResellerLimit) {
		t.Fatalf("over-allocation: want ErrResellerLimit, got %v", err)
	}

	// Lowering always works.
	must(t, svc.SetLimit(ctx, Change{AccountID: subA, NewLimit: dec("0"),
		ActorAccountID: reseller, ActorUser: "admin@reseller", Reason: "overdue"}))

	// A reseller cannot touch accounts outside its own sub-accounts.
	err = svc.SetLimit(ctx, Change{AccountID: direct, NewLimit: dec("10"),
		ActorAccountID: reseller, ActorUser: "admin@reseller", Reason: "x"})
	if !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("reseller on foreign account: want ErrNotAllowed, got %v", err)
	}
	// A customer cannot change limits.
	err = svc.SetLimit(ctx, Change{AccountID: subB, NewLimit: dec("1"),
		ActorAccountID: subA, ActorUser: "u@subA", Reason: "x"})
	if !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("customer actor: want ErrNotAllowed, got %v", err)
	}

	// Direct SQL without actor/reason is refused by the trigger.
	if _, err := pool.Exec(ctx, `UPDATE wallets SET credit_limit = 5 WHERE account_id = $1`, direct); err == nil {
		t.Fatal("credit limit change without app.actor/app.reason must be rejected")
	}

	// Every successful change is audited.
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM credit_limit_changes`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Fatalf("audit rows: want 4, got %d", n)
	}
}

func TestCustomerCannotOwnSubAccounts(t *testing.T) {
	pool := testPool(t)
	platform := newAccount(t, pool, nil, "platform", "Amaze")
	cust := newAccount(t, pool, &platform, "customer", "C")
	_, err := pool.Exec(context.Background(),
		`INSERT INTO accounts (parent_id, role, name) VALUES ($1, 'customer', 'child')`, cust)
	if err == nil {
		t.Fatal("customer as parent must be rejected")
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
