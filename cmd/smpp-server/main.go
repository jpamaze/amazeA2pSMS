// Command smpp-server accepts customer SMPP 3.3/3.4/5.0 binds. A submit_sm is
// answered only after the message is in Kafka (blueprint rule 1).
package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/caarlos0/env/v11"

	"github.com/jpamaze/amazeA2pSMS/internal/auth"
	"github.com/jpamaze/amazeA2pSMS/internal/billing/pricing"
	"github.com/jpamaze/amazeA2pSMS/internal/billing/wallet"
	"github.com/jpamaze/amazeA2pSMS/internal/edge/smppserver"
	"github.com/jpamaze/amazeA2pSMS/internal/ingest"
	"github.com/jpamaze/amazeA2pSMS/internal/msgstore"
	"github.com/jpamaze/amazeA2pSMS/internal/service"
	"github.com/jpamaze/amazeA2pSMS/internal/smpp/pdu"
)

type smppConfig struct {
	Addr          string `env:"SMPP_ADDR" envDefault:":2775"`
	SystemID      string `env:"SMPP_SYSTEM_ID" envDefault:"AMAZE"`
	NodeID        string `env:"NODE_ID"`
	TagPEID       string `env:"DLT_TLV_PE_ID" envDefault:"0x1400"`
	TagTemplateID string `env:"DLT_TLV_TEMPLATE_ID" envDefault:"0x1401"`
	TagTMChain    string `env:"DLT_TLV_TM_CHAIN" envDefault:"0x1402"`
}

func main() {
	var sc smppConfig
	service.Run("smpp-server", service.Needs{Postgres: true, Redis: true, Kafka: true}, func(ctx context.Context, d *service.Deps) error {
		if err := env.Parse(&sc); err != nil {
			return fmt.Errorf("smpp-server config: %w", err)
		}
		if sc.NodeID == "" {
			sc.NodeID, _ = os.Hostname()
		}
		var tags smppserver.DLTTags
		var err error
		if tags.PEID, err = pdu.ParseTag(sc.TagPEID); err != nil {
			return err
		}
		if tags.TemplateID, err = pdu.ParseTag(sc.TagTemplateID); err != nil {
			return err
		}
		if tags.TMChain, err = pdu.ParseTag(sc.TagTMChain); err != nil {
			return err
		}

		store := msgstore.New(d.Redis)
		pipeline := &ingest.Pipeline{
			Pricer:  pricing.NewCached(d.PG, time.Minute),
			Wallets: wallet.New(d.Redis),
			Store:   store,
			Redis:   d.Redis,
			Kafka:   d.Kafka,
		}
		srv := smppserver.New(smppserver.Config{SystemID: sc.SystemID, NodeID: sc.NodeID, DLT: tags},
			auth.New(d.PG), pipeline, store, d.Log)

		ln, err := net.Listen("tcp", sc.Addr)
		if err != nil {
			return err
		}
		d.Log.Info("smpp listening", "addr", sc.Addr, "node", sc.NodeID)
		return srv.Serve(ctx, ln)
	})
}
