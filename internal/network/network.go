// Package network finds the mobile network (MCC/MNC) a number belongs to:
// local MNP data for ported numbers first, then the operator number series,
// then the country default. An HLR lookup (internal/network/hlr.go) can
// override it per route.
//
// MNP data is large (every ported Indian number), so it lives in Redis in
// buckets of 100 numbers (compact small hashes); number series are a few
// thousand prefixes, held in memory and refreshed from PostgreSQL.
package network

import (
	"bufio"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/jpamaze/amazeA2pSMS/internal/numbering"
)

// Network is where a number lives.
type Network struct {
	MCC, MNC string
	Source   string // mnp | series | default | hlr
}

// Series maps national number prefixes to networks.
type Series struct {
	PG  *pgxpool.Pool
	Log *slog.Logger
	m   atomic.Pointer[seriesMap]
}

type seriesMap struct {
	byPrefix map[string]Network // national prefix -> network
	lens     []int              // prefix lengths present, longest first
}

// Load reads number_series.
func (s *Series) Load(ctx context.Context) error {
	rows, err := s.PG.Query(ctx, `SELECT prefix, mcc, mnc FROM number_series`)
	if err != nil {
		return err
	}
	defer rows.Close()
	m := &seriesMap{byPrefix: map[string]Network{}}
	seen := map[int]bool{}
	for rows.Next() {
		var p string
		var n Network
		if err := rows.Scan(&p, &n.MCC, &n.MNC); err != nil {
			return err
		}
		n.Source = "series"
		m.byPrefix[p] = n
		if !seen[len(p)] {
			seen[len(p)] = true
			m.lens = append(m.lens, len(p))
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range m.lens { // longest first
		for j := i + 1; j < len(m.lens); j++ {
			if m.lens[j] > m.lens[i] {
				m.lens[i], m.lens[j] = m.lens[j], m.lens[i]
			}
		}
	}
	s.m.Store(m)
	return nil
}

// Run reloads every interval, keeping the last good map on errors.
func (s *Series) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.Load(ctx); err != nil && s.Log != nil {
				s.Log.Warn("number series not reloaded", "err", err)
			}
		}
	}
}

// Lookup finds the longest prefix of a national number.
func (s *Series) Lookup(national string) (Network, bool) {
	m := s.m.Load()
	if m == nil {
		return Network{}, false
	}
	for _, l := range m.lens {
		if l <= len(national) {
			if n, ok := m.byPrefix[national[:l]]; ok {
				return n, true
			}
		}
	}
	return Network{}, false
}

// Resolver resolves numbers. Series and Redis may be nil.
type Resolver struct {
	Series *Series
	Redis  redis.UniversalClient
}

// India: national numbers are the 10 digits after 91.
func national(e164 string) string { return strings.TrimPrefix(e164, "91") }

// MNP buckets: 8-digit national prefix -> field last 2 digits -> "mcc-mnc".
func mnpKey(nat string) string { return "mnp:" + nat[:8] }

// Resolve returns the number's network. A Redis error falls back to the
// series (MNP only matters for ported numbers) and is returned so the caller
// can count it.
func (r *Resolver) Resolve(ctx context.Context, e164 string) (Network, error) {
	var cmd *redis.StringCmd
	if r.Redis != nil {
		_, _ = r.Redis.Pipelined(ctx, func(p redis.Pipeliner) error { cmd = r.Queue(ctx, p, e164); return nil })
	}
	return r.Result(e164, cmd)
}

// Queue adds the MNP lookup to a pipeline (nil if the number has no MNP
// form), so callers can batch it with other Redis work.
func (r *Resolver) Queue(ctx context.Context, p redis.Pipeliner, e164 string) *redis.StringCmd {
	nat := national(e164)
	if len(nat) != 10 {
		return nil
	}
	return p.HGet(ctx, mnpKey(nat), nat[8:])
}

// Result resolves the number from a queued MNP lookup (cmd may be nil).
func (r *Resolver) Result(e164 string, cmd *redis.StringCmd) (Network, error) {
	nat := national(e164)
	var err error
	if cmd != nil {
		v, e := cmd.Result()
		switch {
		case e == nil:
			if mcc, mnc, ok := strings.Cut(v, "-"); ok {
				return Network{MCC: mcc, MNC: mnc, Source: "mnp"}, nil
			}
		case !errors.Is(e, redis.Nil):
			err = e
		}
	}
	if r.Series != nil {
		if n, ok := r.Series.Lookup(nat); ok {
			return n, err
		}
	}
	return Network{MCC: numbering.IndiaMCC, MNC: "*", Source: "default"}, err
}

// MNPRow is one ported number: its current network, or an empty MNC to
// remove it (ported back to its original network).
type MNPRow struct {
	E164, MCC, MNC string
}

// ImportMNP streams CSV msisdn,mcc,mnc into Redis in pipelines of 1,000.
// A row with an empty mnc deletes the number. It returns rows applied and
// the bad lines (the import keeps going past them).
func ImportMNP(ctx context.Context, rdb redis.UniversalClient, r io.Reader) (int, []string, error) {
	cr := csv.NewReader(bufio.NewReaderSize(r, 1<<20))
	cr.FieldsPerRecord = -1
	cr.ReuseRecord = true
	var bad []string
	n, line := 0, 0
	batch := make([]MNPRow, 0, 1000)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		_, err := rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
			for _, row := range batch {
				nat := national(row.E164)
				if row.MNC == "" {
					p.HDel(ctx, mnpKey(nat), nat[8:])
				} else {
					p.HSet(ctx, mnpKey(nat), nat[8:], row.MCC+"-"+row.MNC)
				}
			}
			return nil
		})
		n += len(batch)
		batch = batch[:0]
		return err
	}
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		line++
		if err != nil {
			return n, bad, fmt.Errorf("CSV line %d: %v", line, err)
		}
		if line == 1 && len(rec) > 0 && strings.EqualFold(strings.TrimSpace(rec[0]), "msisdn") {
			continue
		}
		if len(rec) < 3 {
			bad = appendBad(bad, line, "want msisdn,mcc,mnc")
			continue
		}
		d, err := numbering.NormalizeIndia(rec[0])
		mcc, mnc := strings.TrimSpace(rec[1]), strings.TrimSpace(rec[2])
		if err != nil || (mnc != "" && !ValidMCCMNC(mcc, mnc)) {
			bad = appendBad(bad, line, "bad number, mcc or mnc")
			continue
		}
		batch = append(batch, MNPRow{E164: d.E164, MCC: mcc, MNC: mnc})
		if len(batch) == cap(batch) {
			if err := flush(); err != nil {
				return n, bad, err
			}
		}
	}
	return n, bad, flush()
}

func appendBad(bad []string, line int, why string) []string {
	if len(bad) < 100 {
		bad = append(bad, fmt.Sprintf("line %d: %s", line, why))
	}
	return bad
}

// ValidMCCMNC reports a well-formed MCC (3 digits) and MNC (2 or 3 digits).
func ValidMCCMNC(mcc, mnc string) bool {
	if len(mcc) != 3 || (len(mnc) != 2 && len(mnc) != 3) {
		return false
	}
	for _, r := range mcc + mnc {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
