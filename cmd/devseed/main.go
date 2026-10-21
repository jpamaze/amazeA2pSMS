// Command devseed creates demo data for local development: a platform and a
// customer account with balance, SMPP credentials and an API key, a rate
// card, the simulator vendor with its binds and costs, and a default LCR
// route for India. It is safe to run again; existing rows are reused.
//
// Never run it against production.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/jpamaze/amazeA2pSMS/internal/auth"
	"github.com/jpamaze/amazeA2pSMS/internal/billing/wallet"
	"github.com/jpamaze/amazeA2pSMS/internal/dlt"
	"github.com/jpamaze/amazeA2pSMS/internal/platform/config"
	"github.com/jpamaze/amazeA2pSMS/internal/platform/pg"
	"github.com/jpamaze/amazeA2pSMS/internal/platform/redisx"
	"github.com/jpamaze/amazeA2pSMS/internal/platform/secrets"
)

func main() {
	simHost := flag.String("sim-host", "127.0.0.1", "smpp-sim host")
	simPort := flag.Int("sim-port", 2776, "smpp-sim port")
	simBinds := flag.Int("sim-binds", 4, "transceiver binds to the simulator")
	balance := flag.String("balance", "100000", "demo customer wallet balance (INR)")
	price := flag.String("price", "0.12", "customer price per segment (INR)")
	tps := flag.Int("tps", 10000, "demo SMPP credential TPS limit per bind")
	flag.StringVar(&httpSimURL, "http-sim-url", "http://127.0.0.1:2777", "http-sim base URL")
	flag.StringVar(&routeTo, "route", "sim", "vendor for the demo customer: sim (SMPP, via LCR) or httpsim (HTTP)")
	flag.Parse()

	if err := run(*simHost, *simPort, *simBinds, *balance, *price, *tps); err != nil {
		fmt.Fprintln(os.Stderr, "devseed:", err)
		os.Exit(1)
	}
}

var httpSimURL, routeTo string

// DevDLRToken is the HTTP simulator's callback token in dev data.
const DevDLRToken = "devtoken"

// Dev DLT identities (smpp-load sends DevPEID and the OTP template ID).
const (
	DevPEID = "1101234567890123456"
	DevTMID = "1702000000000000001"
)

func run(simHost string, simPort, simBinds int, balance, price string, tps int) error {
	var cfg config.Common
	if err := config.Load(&cfg); err != nil {
		return err
	}
	if cfg.Env != "dev" && cfg.Env != "staging" {
		return errors.New("refusing to seed: APP_ENV must be dev or staging")
	}
	key := os.Getenv("SECRETS_KEY")
	if key == "" {
		key = secrets.DevKey
	}
	box, err := secrets.New(key)
	if err != nil {
		return err
	}
	ctx := context.Background()
	pool, err := pg.New(ctx, cfg.PostgresURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	rdb := redisx.New(cfg.RedisAddrs)
	defer rdb.Close()

	var platformID, customerID, apiKey string
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		one := func(q string, args ...any) (string, error) {
			var id string
			err := tx.QueryRow(ctx, q, args...).Scan(&id)
			if errors.Is(err, pgx.ErrNoRows) {
				return "", nil
			}
			return id, err
		}

		if platformID, err = one(`SELECT id FROM accounts WHERE role = 'platform' LIMIT 1`); err != nil {
			return err
		}
		if platformID == "" {
			if platformID, err = one(`INSERT INTO accounts (role, name) VALUES ('platform', 'Amaze') RETURNING id`); err != nil {
				return err
			}
		}

		rateCard, err := one(`SELECT id FROM rate_cards WHERE name = 'Default India'`)
		if err != nil {
			return err
		}
		if rateCard == "" {
			if rateCard, err = one(`INSERT INTO rate_cards (name) VALUES ('Default India') RETURNING id`); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO rate_card_entries (rate_card_id, mcc, mnc, price, valid_from)
			VALUES ($1, '404', '*', $2, '2020-01-01') ON CONFLICT (rate_card_id, mcc, mnc, category, valid_from)
			DO UPDATE SET price = EXCLUDED.price`, rateCard, price); err != nil {
			return err
		}

		if customerID, err = one(`SELECT id FROM accounts WHERE name = 'Demo Customer'`); err != nil {
			return err
		}
		if customerID == "" {
			if customerID, err = one(`INSERT INTO accounts (parent_id, role, name, rate_card_id)
				VALUES ($1, 'customer', 'Demo Customer', $2) RETURNING id`, platformID, rateCard); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE wallets SET balance = $2 WHERE account_id = $1`, customerID, balance); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO ledger_entries (account_id, kind, amount, balance_after, reference, idempotency_key, created_by)
				VALUES ($1, 'topup', $2, $2, 'devseed opening balance', $3, 'devseed')`, customerID, balance, "devseed:"+customerID); err != nil {
				return err
			}
		}

		// DLT registrations matching smpp-load's default message, plus a test
		// template for scripts: "Test message <anything up to 30 chars>".
		if _, err := tx.Exec(ctx, `INSERT INTO telemarketers (tm_id, name, is_self) VALUES ($1, 'Amaze (dev)', true)
			ON CONFLICT (tm_id) DO NOTHING`, DevTMID); err != nil {
			return err
		}
		entityID, err := one(`INSERT INTO dlt_entities (account_id, pe_id, name) VALUES ($1, $2, 'Demo Customer')
			ON CONFLICT (pe_id) DO UPDATE SET status = 'active' RETURNING id`, customerID, DevPEID)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO dlt_tm_chains (entity_id, chain_value)
			SELECT $1, $2 WHERE NOT EXISTS (SELECT 1 FROM dlt_tm_chains WHERE entity_id = $1)`, entityID, DevPEID+","+DevTMID); err != nil {
			return err
		}
		if err := dlt.StoreHeaders(ctx, tx, entityID, []dlt.HeaderRow{{Header: "AMZTST", Category: dlt.Transactional, Status: "active"}}); err != nil {
			return err
		}
		if err := dlt.StoreTemplates(ctx, tx, entityID, []dlt.TemplateRow{
			{TemplateID: "1107160000000012345", Name: "OTP", Category: dlt.Transactional, Status: "active", Headers: []string{"AMZTST"},
				Body: "Your OTP is {#var#}. Do not share it with anyone. -Amaze"},
			{TemplateID: "1107160000000012346", Name: "Test", Category: dlt.Transactional, Status: "active", Headers: []string{"AMZTST"},
				Body: "Test message {#var#}"},
		}); err != nil {
			return err
		}

		// Platform owner who signs in to the admin API through the dev Keycloak.
		if _, err := tx.Exec(ctx, `INSERT INTO users (account_id, email, role) VALUES ($1, 'ops@amaze.local', 'owner')
			ON CONFLICT ((lower(email))) DO NOTHING`, platformID); err != nil {
			return err
		}

		hash, err := auth.HashPassword("demo123")
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO smpp_credentials (account_id, system_id, password_hash, max_binds, tps_limit)
			VALUES ($1, 'demo', $2, 16, $3)
			ON CONFLICT (system_id) DO UPDATE SET password_hash = EXCLUDED.password_hash, tps_limit = EXCLUDED.tps_limit`,
			customerID, hash, tps); err != nil {
			return err
		}

		full, prefix, keyHash, err := auth.NewAPIKey()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE api_keys SET revoked_at = now() WHERE account_id = $1 AND name = 'devseed' AND revoked_at IS NULL`, customerID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO api_keys (account_id, prefix, key_hash, name) VALUES ($1, $2, $3, 'devseed')`,
			customerID, prefix, keyHash); err != nil {
			return err
		}
		apiKey = full

		if _, err := tx.Exec(ctx, `INSERT INTO vendors (id, name, protocol) VALUES ('sim', 'SMPP simulator', 'smpp')
			ON CONFLICT (id) DO UPDATE SET status = 'active'`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM vendor_binds WHERE vendor_id = 'sim'`); err != nil {
			return err
		}
		pw, err := box.Seal([]byte("sim"))
		if err != nil {
			return err
		}
		for range simBinds {
			if _, err := tx.Exec(ctx, `INSERT INTO vendor_binds (vendor_id, host, port, system_id, password_enc,
				smpp_version, bind_type, window_size, tps_limit, tlv_profile)
				VALUES ('sim', $1, $2, 'amaze', $3, '3.4', 'transceiver', 100, 5000,
				'{"pe_id": "0x1400", "template_id": "0x1401", "tm_chain": "0x1402"}')`, simHost, simPort, pw); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO vendor_costs (vendor_id, mcc, mnc, cost, valid_from)
			VALUES ('sim', '404', '*', 0.05, '2020-01-01') ON CONFLICT DO NOTHING`); err != nil {
			return err
		}
		// HTTP vendor simulator (dearer than sim, so LCR prefers sim).
		if _, err := tx.Exec(ctx, `INSERT INTO vendors (id, name, protocol) VALUES ('httpsim', 'HTTP simulator', 'http')
			ON CONFLICT (id) DO UPDATE SET status = 'active'`); err != nil {
			return err
		}
		key, err := box.Seal([]byte("simkey"))
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO http_vendor_profiles
			(vendor_id, base_url, auth, request_template, response_mapping, dlr_mapping, tps_limit, secret_enc, dlr_token, concurrency)
			VALUES ('httpsim', $1, '{"type":"bearer"}',
			  '{"path":"/send","encoding":"json","params":{"to":"{{to_national}}","from":"{{sender}}","msg":"{{text}}","entity":"{{pe_id}}","tpl":"{{template_id}}","uni":"{{unicode}}","ref":"{{id}}"}}',
			  '{"success_path":"status","success_values":["success"],"id_path":"data.0.msgid","error_path":"code"}',
			  '{"id_param":"msgid","status_param":"status","error_param":"err","delivered_values":["DELIVERED"],"expired_values":["EXPIRED"]}',
			  5000, $2, $3, 200)
			ON CONFLICT (vendor_id) DO UPDATE SET base_url = EXCLUDED.base_url, secret_enc = EXCLUDED.secret_enc,
			  dlr_token = EXCLUDED.dlr_token`, httpSimURL, key, DevDLRToken); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO vendor_costs (vendor_id, mcc, mnc, cost, valid_from)
			VALUES ('httpsim', '404', '*', 0.06, '2020-01-01') ON CONFLICT DO NOTHING`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM routes WHERE account_id = $1`, customerID); err != nil {
			return err
		}
		if routeTo == "httpsim" {
			if _, err := tx.Exec(ctx, `INSERT INTO routes (account_id, mcc, mnc, mode, vendor_id, priority)
				VALUES ($1, '404', '*', 'priority', 'httpsim', 1)`, customerID); err != nil {
				return err
			}
		}

		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM routes WHERE account_id IS NULL AND mcc = '404'`).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			if _, err := tx.Exec(ctx, `INSERT INTO routes (mcc, mnc, mode, priority) VALUES ('404', '*', 'lcr', 100)`); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}

	// Load the customer's wallet into Redis from the system of record.
	var bal, limit decimal.Decimal
	if err := pool.QueryRow(ctx, `SELECT balance, credit_limit FROM wallets WHERE account_id = $1`, customerID).Scan(&bal, &limit); err != nil {
		return err
	}
	if err := wallet.New(rdb).Sync(ctx, customerID, wallet.FromDecimal(bal), wallet.FromDecimal(limit)); err != nil {
		return err
	}

	fmt.Printf(`Seeded.
  DLT              : PE %s, header AMZTST, templates "Your OTP is {#var#}. Do not share it with anyone. -Amaze" and "Test message {#var#}"
  customer account : %s
  wallet balance   : %s INR (credit limit %s)
  SMPP             : system_id=demo password=demo123 (port 2775)
  HTTP API key     : %s
  vendor           : sim -> %s:%d (%d binds); httpsim -> %s (DLR token %q)
  demo routed to   : %s
`, DevPEID, customerID, bal.StringFixed(2), limit.StringFixed(2), apiKey, simHost, simPort, simBinds, httpSimURL, DevDLRToken, routeTo)
	return nil
}
