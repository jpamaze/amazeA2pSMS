package main

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/caarlos0/env/v11"

	"github.com/jpamaze/amazeA2pSMS/internal/domain"
	"github.com/jpamaze/amazeA2pSMS/internal/network"
	"github.com/jpamaze/amazeA2pSMS/internal/platform/metrics"
	"github.com/jpamaze/amazeA2pSMS/internal/routing"
	"github.com/jpamaze/amazeA2pSMS/internal/service"
)

type hlrEnv struct {
	URL           string        `env:"HLR_URL"`
	AuthHeader    string        `env:"HLR_AUTH_HEADER"`
	AuthValue     string        `env:"HLR_AUTH_VALUE"`
	MCCPath       string        `env:"HLR_MCC_PATH"`
	MNCPath       string        `env:"HLR_MNC_PATH"`
	MCCMNCPath    string        `env:"HLR_MCCMNC_PATH"`
	StatusPath    string        `env:"HLR_STATUS_PATH"`
	StatusOK      []string      `env:"HLR_STATUS_OK" envSeparator:","`
	StatusAbsent  []string      `env:"HLR_STATUS_ABSENT" envSeparator:","`
	StatusInvalid []string      `env:"HLR_STATUS_INVALID" envSeparator:","`
	Timeout       time.Duration `env:"HLR_TIMEOUT" envDefault:"2s"`
	CacheTTL      time.Duration `env:"HLR_CACHE_TTL" envDefault:"48h"`
	Concurrency   int           `env:"HLR_CONCURRENCY" envDefault:"64"`
}

type hlrStep struct {
	hlr         *network.HLR
	concurrency int
}

func newHLR(d *service.Deps) (*hlrStep, error) {
	var e hlrEnv
	if err := env.Parse(&e); err != nil {
		return nil, err
	}
	return &hlrStep{concurrency: max(e.Concurrency, 1), hlr: &network.HLR{Redis: d.Redis, Cfg: network.HLRConfig{
		URL: e.URL, AuthHeader: e.AuthHeader, AuthValue: e.AuthValue, MCCPath: e.MCCPath, MNCPath: e.MNCPath,
		MCCMNCPath: e.MCCMNCPath, StatusPath: e.StatusPath, StatusOK: e.StatusOK, StatusAbsent: e.StatusAbsent,
		StatusInvalid: e.StatusInvalid, Timeout: e.Timeout, CacheTTL: e.CacheTTL,
	}}}, nil
}

// apply looks up the batch's numbers whose route asks for HLR (cache first,
// then the provider, concurrently), corrects their network, and returns the
// error code for messages to refuse. A failed lookup never blocks a
// message: it is routed on its MNP/series network.
func (s *hlrStep) apply(ctx context.Context, t *routing.Table, msgs []*domain.Message) map[int]string {
	type want struct {
		i      int
		reject bool
	}
	var wants []want
	var numbers []string
	for i, m := range msgs {
		if m == nil || len(m.Avoid) > 0 { // rerouted: looked up the first time
			continue
		}
		if lookup, reject := t.HLRPolicy(m.AccountID, m.MCC); lookup {
			wants = append(wants, want{i, reject})
			numbers = append(numbers, m.Destination)
		}
	}
	refuse := map[int]string{}
	if len(wants) == 0 {
		return refuse
	}
	cached, _ := s.hlr.Cached(ctx, numbers)
	results := make([]*network.HLRResult, len(wants))
	var wg sync.WaitGroup
	sem := make(chan struct{}, s.concurrency)
	for k, w := range wants {
		if r, ok := cached[msgs[w.i].Destination]; ok {
			metrics.HLRLookups.WithLabelValues("cache", r.Status).Inc()
			results[k] = &r
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(k int, number string) {
			defer wg.Done()
			defer func() { <-sem }()
			r, err := s.hlr.Lookup(ctx, number)
			if err != nil {
				metrics.HLRLookups.WithLabelValues("provider", "error").Inc()
				return
			}
			metrics.HLRLookups.WithLabelValues("provider", r.Status).Inc()
			results[k] = &r
		}(k, msgs[w.i].Destination)
	}
	wg.Wait()
	for k, w := range wants {
		r := results[k]
		if r == nil {
			continue
		}
		m := msgs[w.i]
		if w.reject && (r.Status == network.HLRInvalid || r.Status == network.HLRAbsent) {
			refuse[w.i] = "HLR_" + strings.ToUpper(r.Status)
			continue
		}
		if r.MCC != "" {
			m.MCC, m.MNC = r.MCC, r.MNC
		}
	}
	return refuse
}
