// Command connector holds outbound connections to vendors and sends routed
// messages, honouring per-bind TPS, window and SMPP version. DLRs are written
// to Kafka before deliver_sm_resp (blueprint rule 3).
//
// VENDOR_ID=* (the default) runs every vendor that is not disabled: vendors
// added in the portal start within VENDOR_RESCAN, and a vendor whose binds or
// HTTP profile change is restarted with the new settings. VENDOR_ID=<id> runs
// that one vendor only.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/caarlos0/env/v11"
	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/jpamaze/amazeA2pSMS/internal/connector"
	"github.com/jpamaze/amazeA2pSMS/internal/msgstore"
	"github.com/jpamaze/amazeA2pSMS/internal/platform/kafka"
	"github.com/jpamaze/amazeA2pSMS/internal/platform/metrics"
	"github.com/jpamaze/amazeA2pSMS/internal/platform/secrets"
	"github.com/jpamaze/amazeA2pSMS/internal/service"
	"github.com/jpamaze/amazeA2pSMS/internal/smpp/pdu"
	"github.com/jpamaze/amazeA2pSMS/internal/vendorhealth"
)

type connectorConfig struct {
	VendorID   string `env:"VENDOR_ID" envDefault:"*"`
	SecretsKey string `env:"SECRETS_KEY"`
	// HTTP vendors call back delivery receipts here: /dlr/<vendor>?token=<dlr_token>
	DLRAddr string `env:"DLR_ADDR" envDefault:":8090"`
	// With VENDOR_ID=*: how often the vendor list is read, and how new
	// vendors' route topics are created (-1 = the broker's default).
	Rescan          time.Duration `env:"VENDOR_RESCAN" envDefault:"30s"`
	TopicPartitions int32         `env:"ROUTE_TOPIC_PARTITIONS" envDefault:"24"`
	TopicReplicas   int16         `env:"ROUTE_TOPIC_REPLICAS" envDefault:"-1"`
}

func main() {
	var cc connectorConfig
	service.Run("connector", service.Needs{Postgres: true, Redis: true, Kafka: true}, func(ctx context.Context, d *service.Deps) error {
		if err := env.Parse(&cc); err != nil {
			return fmt.Errorf("connector config: %w", err)
		}
		box, err := secrets.FromEnv(cc.SecretsKey, d.Cfg.Env)
		if err != nil {
			return err
		}
		dlr := &dlrRoutes{m: map[string]http.Handler{}}
		srv := &http.Server{Addr: cc.DLRAddr, Handler: dlr, ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second}
		dlrServer := sync.OnceFunc(func() {
			go func() {
				d.Log.Info("HTTP DLR endpoint listening", "addr", cc.DLRAddr, "path", "/dlr/<vendor>")
				if err := service.ServeHTTP(ctx, srv); err != nil {
					d.Log.Error("DLR endpoint stopped", "err", err)
				}
			}()
		})
		if cc.VendorID != "*" {
			return runVendor(ctx, d, box, cc.VendorID, dlr, dlrServer, true)
		}
		return supervise(ctx, d, box, cc, dlr, dlrServer)
	})
}

// dlrRoutes sends /dlr/<vendor> to that vendor's receipt handler while the
// vendor runs.
type dlrRoutes struct {
	mu sync.RWMutex
	m  map[string]http.Handler
}

func (r *dlrRoutes) set(vendor string, h http.Handler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if h == nil {
		delete(r.m, vendor)
	} else {
		r.m[vendor] = h
	}
}

func (r *dlrRoutes) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	vendor, ok := strings.CutPrefix(req.URL.Path, "/dlr/")
	r.mu.RLock()
	h := r.m[vendor]
	r.mu.RUnlock()
	if !ok || h == nil {
		http.NotFound(w, req)
		return
	}
	h.ServeHTTP(w, req)
}

// runVendor sends one vendor's routed messages until ctx ends or consuming
// fails. bindCheck adds a readiness check that fails while no bind is up
// (single-vendor mode only: one vendor being down must not take the whole
// process out).
func runVendor(ctx context.Context, d *service.Deps, box *secrets.Box, vendorID string, dlr *dlrRoutes, dlrServer func(), bindCheck bool) error {
	log := d.Log.With("vendor", vendorID)
	c := &connector.Connector{
		VendorID: vendorID, Store: msgstore.New(d.Redis), Kafka: d.Kafka, Log: log,
		Health: &vendorhealth.Recorder{},
	}
	var down atomic.Bool
	c.Down = down.Load
	var protocol string
	if err := d.PG.QueryRow(ctx, `SELECT protocol FROM vendors WHERE id = $1`, vendorID).Scan(&protocol); err != nil {
		return fmt.Errorf("vendor %s: %w", vendorID, err)
	}
	vd := *d
	vd.Log = log

	consume := func() error {
		go watchHealth(ctx, &vd, c, &down) // after c.Transport is set
		return service.ConsumeBatch(ctx, &vd, "connector-"+vendorID,
			[]string{kafka.RouteTopic(vendorID)}, 4*c.Transport.Window(),
			func(ctx context.Context, recs []*kgo.Record) error { return c.Process(ctx, recs) })
	}

	if protocol == "http" {
		p, err := loadHTTPProfile(ctx, d, box, vendorID)
		if err != nil {
			return err
		}
		c.Transport = connector.NewHTTPTransport(p)
		dlr.set(vendorID, connector.DLRHandler(p, c.JournalDLR, log))
		defer dlr.set(vendorID, nil)
		dlrServer()
		return consume()
	}

	binds, profile, err := loadBinds(ctx, d, box, vendorID)
	if err != nil {
		return err
	}
	bctx, stop := context.WithCancel(ctx)
	var running sync.WaitGroup
	defer func() { // unbind when this vendor stops, and drop its gauge
		stop()
		running.Wait()
		metrics.VendorBindsUp.DeleteLabelValues(vendorID)
	}()
	var pool []*connector.Bind
	for _, bc := range binds {
		b := connector.NewBind(vendorID, bc, c.Deliver, log)
		pool = append(pool, b)
		running.Go(func() { b.Run(bctx) })
	}
	bp := connector.NewPool(pool)
	bp.Down = down.Load
	c.Transport = &connector.SMPPTransport{Pool: bp, TLVs: profile}
	if bindCheck {
		d.Health.Add("vendor-bind", func(context.Context) error {
			for _, b := range pool {
				if b.Up() {
					return nil
				}
			}
			return fmt.Errorf("no bind to %s is up", vendorID)
		})
	}
	return consume()
}

// vendorFingerprints lists the vendors to run (not disabled, and with an
// enabled bind or an HTTP profile) with a digest of everything their
// connection is built from, so an edit restarts the vendor.
const vendorFingerprints = `
	SELECT v.id, md5(v.protocol
	  || coalesce((SELECT string_agg(b::text, '|' ORDER BY b.id) FROM vendor_binds b WHERE b.vendor_id = v.id AND b.enabled), '')
	  || coalesce((SELECT p::text FROM http_vendor_profiles p WHERE p.vendor_id = v.id), ''))
	FROM vendors v WHERE v.status <> 'disabled'
	  AND (EXISTS (SELECT 1 FROM vendor_binds b WHERE b.vendor_id = v.id AND b.enabled)
	       OR EXISTS (SELECT 1 FROM http_vendor_profiles p WHERE p.vendor_id = v.id))
	ORDER BY v.id`

type worker struct {
	fp     string
	cancel context.CancelFunc
	done   chan struct{}
}

// supervise runs every vendor that is not disabled and follows changes.
func supervise(ctx context.Context, d *service.Deps, box *secrets.Box, cc connectorConfig, dlr *dlrRoutes, dlrServer func()) error {
	admin := kadm.NewClient(d.Kafka)
	topics := map[string]bool{}
	running := map[string]*worker{}
	stop := func(id string) {
		w := running[id]
		w.cancel()
		<-w.done
		delete(running, id)
	}
	defer func() {
		for id := range running {
			stop(id)
		}
	}()
	tick := time.NewTicker(cc.Rescan)
	defer tick.Stop()
	for {
		want, err := scanVendors(ctx, d)
		if err != nil {
			d.Log.Warn("vendor list not read; keeping the running vendors", "err", err)
		} else {
			for id := range running {
				if _, ok := want[id]; !ok {
					d.Log.Info("vendor disabled or removed; stopping it", "vendor", id)
					stop(id)
				}
			}
			for id, fp := range want {
				if w, ok := running[id]; ok {
					select {
					case <-w.done: // exited with an error: start again below
						delete(running, id)
					default:
						if w.fp == fp {
							continue
						}
						d.Log.Info("vendor settings changed; restarting it", "vendor", id)
						stop(id)
					}
				}
				if !topics[id] {
					if err := ensureTopic(ctx, admin, kafka.RouteTopic(id), cc.TopicPartitions, cc.TopicReplicas); err != nil {
						d.Log.Error("route topic not created; vendor not started", "vendor", id, "err", err)
						continue
					}
					topics[id] = true
				}
				wctx, cancel := context.WithCancel(ctx)
				w := &worker{fp: fp, cancel: cancel, done: make(chan struct{})}
				running[id] = w
				go func(id string) {
					defer close(w.done)
					if err := runVendor(wctx, d, box, id, dlr, dlrServer, false); err != nil && wctx.Err() == nil {
						d.Log.Error("vendor stopped; retrying at the next rescan", "vendor", id, "err", err)
					}
				}(id)
			}
			d.Log.Debug("vendors running", "count", len(running))
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

func scanVendors(ctx context.Context, d *service.Deps) (map[string]string, error) {
	rows, err := d.PG.Query(ctx, vendorFingerprints)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, fp string
		if err := rows.Scan(&id, &fp); err != nil {
			return nil, err
		}
		out[id] = fp
	}
	return out, rows.Err()
}

// ensureTopic creates a vendor's route topic with the settings of
// deploy/kafka-topics.sh (brokers do not create topics on their own).
func ensureTopic(ctx context.Context, admin *kadm.Client, topic string, partitions int32, replicas int16) error {
	retention, zstd, isr := fmt.Sprint(72*3600*1000), "zstd", "2"
	configs := map[string]*string{"retention.ms": &retention, "compression.type": &zstd}
	if replicas > 1 { // with the broker's default (-1) its own min.insync.replicas applies
		configs["min.insync.replicas"] = &isr
	}
	res, err := admin.CreateTopic(ctx, partitions, replicas, configs, topic)
	if err == nil {
		err = res.Err
	}
	if errors.Is(err, kerr.TopicAlreadyExists) {
		return nil
	}
	return err
}

// loadHTTPProfile reads and validates an HTTP vendor's profile.
func loadHTTPProfile(ctx context.Context, d *service.Deps, box *secrets.Box, vendorID string) (connector.HTTPProfile, error) {
	var (
		base                 string
		auth, req, resp, dlr []byte
		secretEnc            []byte
		token                string
		tps, concurrency     int
	)
	err := d.PG.QueryRow(ctx, `SELECT base_url, auth, request_template, response_mapping, dlr_mapping,
		secret_enc, dlr_token, tps_limit, concurrency FROM http_vendor_profiles WHERE vendor_id = $1`, vendorID).
		Scan(&base, &auth, &req, &resp, &dlr, &secretEnc, &token, &tps, &concurrency)
	if err != nil {
		return connector.HTTPProfile{}, fmt.Errorf("http vendor %s profile: %w", vendorID, err)
	}
	p, err := connector.ParseHTTPProfile(base, auth, req, resp, dlr)
	if err != nil {
		return p, err
	}
	if len(secretEnc) > 0 {
		sec, err := box.Open(secretEnc)
		if err != nil {
			return p, fmt.Errorf("http vendor %s secret: %w", vendorID, err)
		}
		p.Secret = string(sec)
	}
	p.DLRToken, p.TPS, p.Concurrency = token, tps, concurrency
	return p, nil
}

// loadBinds reads the vendor's enabled binds and decrypts their passwords.
// All binds of a vendor share the TLV profile of the first bind.
func loadBinds(ctx context.Context, d *service.Deps, box *secrets.Box, vendorID string) ([]connector.BindConfig, connector.TLVProfile, error) {
	var profile connector.TLVProfile
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

// watchHealth publishes this instance's view of the vendor every two
// seconds and follows the routers' failover verdict every second.
func watchHealth(ctx context.Context, d *service.Deps, c *connector.Connector, down *atomic.Bool) {
	host, _ := os.Hostname()
	instance := fmt.Sprintf("%s-%d", host, os.Getpid())
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for n := 0; ; n++ {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if n%2 == 0 {
			up, total := 1, 0 // HTTP vendors have no binds
			if bc, ok := c.Transport.(interface{ Binds() (int, int) }); ok {
				up, total = bc.Binds()
			}
			if err := vendorhealth.Publish(ctx, d.Redis, c.VendorID, instance, c.Health.Snapshot(up, total)); err != nil {
				d.Log.Warn("vendor health not published", "err", err)
			}
		}
		states, err := vendorhealth.States(ctx, d.Redis)
		if err != nil {
			continue // keep the last verdict
		}
		st, ok := states[c.VendorID]
		isDown := ok && !st.Up
		if down.Swap(isDown) != isDown {
			d.Log.Warn("failover verdict changed; unsent messages go to other routes while down", "down", isDown, "reason", st.Reason)
		}
	}
}
