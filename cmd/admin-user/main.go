// Command admin-user grants a person sign-in access to an account (by
// default the platform account). Use it to create the first platform owner;
// after that, owners and admins grant access through the admin API.
//
//	admin-user -email ops@example.com -role owner
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/jpamaze/amazeA2pSMS/internal/platform/config"
	"github.com/jpamaze/amazeA2pSMS/internal/platform/pg"
)

func main() {
	email := flag.String("email", "", "the person's email, exactly as in Keycloak")
	role := flag.String("role", "owner", "owner | admin | finance | support | viewer")
	account := flag.String("account", "", "account ID (default: the platform account)")
	flag.Parse()
	if !strings.Contains(*email, "@") {
		fmt.Fprintln(os.Stderr, "-email is required")
		os.Exit(2)
	}
	var cfg config.Common
	if err := config.Load(&cfg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	ctx := context.Background()
	pool, err := pg.New(ctx, cfg.PostgresURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer pool.Close()
	acct := *account
	if acct == "" {
		if err := pool.QueryRow(ctx, `SELECT id FROM accounts WHERE role = 'platform'`).Scan(&acct); err != nil {
			fmt.Fprintln(os.Stderr, "no platform account (run migrate and create it first):", err)
			os.Exit(1)
		}
	}
	_, err = pool.Exec(ctx, `INSERT INTO users (account_id, email, role) VALUES ($1, lower($2), $3)
		ON CONFLICT ((lower(email))) DO UPDATE SET role = EXCLUDED.role, account_id = EXCLUDED.account_id, disabled_at = NULL`,
		acct, *email, *role)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("%s can now sign in to account %s as %s\n", strings.ToLower(*email), acct, *role)
}
