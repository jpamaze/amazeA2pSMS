// Command smpp-server accepts customer SMPP 3.3/3.4/5.0 binds. A submit_sm is
// answered only after the message is in Kafka (blueprint rule 1).
package main

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/caarlos0/env/v11"

	"github.com/jpamaze/amazeA2pSMS/internal/service"
)

type smppConfig struct {
	Addr string `env:"SMPP_ADDR" envDefault:":2775"`
}

func main() {
	var sc smppConfig
	service.Run("smpp-server", service.Needs{Postgres: true, Redis: true, Kafka: true}, func(ctx context.Context, d *service.Deps) error {
		if err := env.Parse(&sc); err != nil {
			return fmt.Errorf("smpp-server config: %w", err)
		}
		ln, err := net.Listen("tcp", sc.Addr)
		if err != nil {
			return err
		}
		go func() { <-ctx.Done(); ln.Close() }()
		d.Log.Info("smpp listening", "addr", sc.Addr)

		for {
			conn, err := ln.Accept()
			if err != nil {
				if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
					return nil
				}
				d.Log.Warn("accept", "err", err)
				continue
			}
			// TODO(phase 1): PDU codec, bind auth against smpp_credentials,
			// per-bind token bucket and window, enquire_link watchdog, TLVs.
			d.Log.Info("smpp connection refused: protocol not implemented yet", "remote", conn.RemoteAddr().String())
			conn.Close()
		}
	})
}
