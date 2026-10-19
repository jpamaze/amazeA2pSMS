// Command connector holds outbound SMPP binds to one vendor and sends routed
// messages, honouring per-bind TPS, window and SMPP version. DLRs are written
// to Kafka before deliver_sm_resp (blueprint rule 3).
package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/caarlos0/env/v11"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/jpamaze/amazeA2pSMS/internal/connector"
	"github.com/jpamaze/amazeA2pSMS/internal/msgstore"
	"github.com/jpamaze/amazeA2pSMS/internal/platform/kafka"
	"github.com/jpamaze/amazeA2pSMS/internal/platform/secrets"
	"github.com/jpamaze/amazeA2pSMS/internal/service"
	"github.com/jpamaze/amazeA2pSMS/internal/smpp/pdu"
)

type connectorConfig struct {
	VendorID   string `env:"VENDOR_ID,required"`
	SecretsKey string `env:"SECRETS_KEY"`
}

func main() {
	var cc connectorConfig
	service.Run("connector", service.Needs{Postgres: true, Redis: true, Kafka: true}, func(ctx context.Context, d *service.Deps) error {
		if err := env.Parse(&cc); err != nil {
			return fmt.Errorf("connector config: %w", err)
		}
		if cc.SecretsKey == "" {
			if d.Cfg.Env != "dev" {
				return fmt.Errorf("SECRETS_KEY is required outside APP_ENV=dev")
			}
			cc.SecretsKey = secrets.DevKey
		}
		box, err := secrets.New(cc.SecretsKey)
		if err != nil {
			return err
		}
		d.Log = d.Log.With("vendor", cc.VendorID)

		binds, profile, err := loadBinds(ctx, d, box, cc.VendorID)
		if err != nil {
			return err
		}
		c := &connector.Connector{
			VendorID: cc.VendorID, TLVs: profile,
			Store: msgstore.New(d.Redis), Kafka: d.Kafka, Log: d.Log,
		}
		var pool []*connector.Bind
		for _, bc := range binds {
			b := connector.NewBind(bc, c.Deliver, d.Log)
			pool = append(pool, b)
			go b.Run(ctx)
		}
		c.Pool = connector.NewPool(pool)
		d.Health.Add("vendor-bind", func(context.Context) error {
			for _, b := range pool {
				if b.Up() {
					return nil
				}
			}
			return fmt.Errorf("no bind to %s is up", cc.VendorID)
		})

		return service.ConsumeBatch(ctx, d, "connector-"+cc.VendorID,
			[]string{kafka.RouteTopic(cc.VendorID)}, 4*c.Pool.Window(),
			func(ctx context.Context, recs []*kgo.Record) error { return c.Process(ctx, recs) })
	})
}

// loadBinds reads the vendor's enabled binds and decrypts their passwords.
// All binds of a vendor share the TLV profile of the first bind.
func loadBinds(ctx context.Context, d *service.Deps, box *secrets.Box, vendorID string) ([]connector.BindConfig, connector.TLVProfile, error) {
	var profile connector.TLVProfile
	var protocol string
	if err := d.PG.QueryRow(ctx, `SELECT protocol FROM vendors WHERE id = $1`, vendorID).Scan(&protocol); err != nil {
		return nil, profile, fmt.Errorf("vendor %s: %w", vendorID, err)
	}
	if protocol != "smpp" {
		return nil, profile, fmt.Errorf("vendor %s uses %s; only SMPP connectors exist in this release", vendorID, protocol)
	}
	rows, err := d.PG.Query(ctx, `
		SELECT id, host, port, tls, system_id, password_enc, system_type, smpp_version, bind_type,
		       window_size, tps_limit, tlv_profile
		FROM vendor_binds WHERE vendor_id = $1 AND enabled ORDER BY id`, vendorID)
	if err != nil {
		return nil, profile, err
	}
	defer rows.Close()
	var out []connector.BindConfig
	for rows.Next() {
		var (
			b       connector.BindConfig
			enc     []byte
			version string
			tlvJSON []byte
		)
		if err := rows.Scan(&b.ID, &b.Host, &b.Port, &b.TLS, &b.SystemID, &enc, &b.SystemType, &version,
			&b.BindType, &b.Window, &b.TPS, &tlvJSON); err != nil {
			return nil, profile, err
		}
		pw, err := box.Open(enc)
		if err != nil {
			return nil, profile, fmt.Errorf("bind %s password: %w", b.ID, err)
		}
		b.Password = string(pw)
		switch version {
		case "3.3":
			b.Version = pdu.V33
		case "5.0":
			b.Version = pdu.V50
		default:
			b.Version = pdu.V34
		}
		var raw map[string]string
		if len(tlvJSON) > 0 {
			if err := json.Unmarshal(tlvJSON, &raw); err != nil {
				return nil, profile, fmt.Errorf("bind %s tlv_profile: %w", b.ID, err)
			}
		}
		parse := func(k string) uint16 {
			if raw[k] == "" {
				return 0
			}
			t, err := pdu.ParseTag(raw[k])
			if err != nil {
				d.Log.Warn("bad TLV tag in profile", "bind", b.ID, "key", k, "err", err)
			}
			return t
		}
		b.TLVs = connector.TLVProfile{PEID: parse("pe_id"), TemplateID: parse("template_id"), TMChain: parse("tm_chain")}
		if len(out) == 0 {
			profile = b.TLVs
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, profile, err
	}
	if len(out) == 0 {
		return nil, profile, fmt.Errorf("vendor %s has no enabled binds", vendorID)
	}
	return out, profile, nil
}
