// Package firewall refuses abusive traffic at submit: senders, content,
// links and destinations on a block list, and traffic patterns that look
// like SMS pumping (artificially inflated traffic to number ranges held on
// SIM farms) or destination flooding.
//
// Rules are held in memory and reloaded every 30 seconds. Velocity is
// counted in Redis with one script call per message.
package firewall

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/jpamaze/amazeA2pSMS/internal/dlt"
	"github.com/jpamaze/amazeA2pSMS/internal/platform/metrics"
)

// Reason is why the firewall refused a message (HTTP code, metrics label).
type Reason string

// Reasons with their SMPP command_status.
const (
	ReasonSender      Reason = "fw_sender"           // sender on a block list
	ReasonContent     Reason = "fw_content"          // text matches a content rule
	ReasonURL         Reason = "fw_url"              // link to a blocked domain
	ReasonDestination Reason = "fw_destination"      // number or range on a block list
	ReasonDestRate    Reason = "fw_destination_rate" // too many messages to one number
	ReasonSeries      Reason = "fw_series"           // number range blocked for suspected pumping
)

var smppStatus = map[Reason]uint32{
	ReasonSender: 0x420, ReasonContent: 0x421, ReasonURL: 0x422, ReasonDestination: 0x423,
	ReasonDestRate: 0x424, ReasonSeries: 0x425,
}

// Error is a firewall refusal.
type Error struct {
	Reason Reason
	Detail string
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %s", e.Reason, e.Detail) }

// SMPPStatus is the submit_sm_resp status for the refusal.
func (e *Error) SMPPStatus() uint32 { return smppStatus[e.Reason] }

// AsError returns the *Error inside err, if any.
func AsError(err error) (*Error, bool) {
	var e *Error
	ok := errors.As(err, &e)
	return e, ok
}

// Rule kinds.
const (
	KindSender      = "sender"      // glob on the sender, case-insensitive (* and ?)
	KindContent     = "content"     // phrase (whole words, case-insensitive) or re:<regexp>
	KindURL         = "url"         // domain: blocks links to it and its subdomains
	KindDestination = "destination" // glob on the E.164 number, e.g. 9198100*
)

// Rule is one firewall rule.
type Rule struct {
	ID        int64
	AccountID string // "" = every account
	Kind      string
	Pattern   string
	Action    string // reject | flag (count only)
	re        *regexp.Regexp
}

// Compile checks a rule and prepares it for matching.
func (r *Rule) Compile() error {
	p := strings.TrimSpace(r.Pattern)
	if p == "" {
		return errors.New("pattern is empty")
	}
	if r.Action != "reject" && r.Action != "flag" {
		return fmt.Errorf("action must be reject or flag")
	}
	var expr string
	switch r.Kind {
	case KindSender, KindDestination:
		expr = "(?i)^" + strings.NewReplacer(`\*`, ".*", `\?`, ".").Replace(regexp.QuoteMeta(p)) + "$"
	case KindContent:
		if rest, ok := strings.CutPrefix(p, "re:"); ok {
			expr = "(?i)" + rest
		} else {
			expr = `(?i)(^|\W)` + strings.Join(strings.Fields(regexp.QuoteMeta(p)), `\s+`) + `($|\W)`
		}
	case KindURL:
		host := strings.ToLower(strings.TrimPrefix(strings.TrimPrefix(p, "https://"), "http://"))
		host, _, _ = strings.Cut(host, "/")
		r.Pattern = strings.TrimPrefix(host, "www.")
		return nil
	default:
		return fmt.Errorf("unknown kind %q", r.Kind)
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return fmt.Errorf("bad pattern: %v", err)
	}
	r.re = re
	return nil
}

func (r *Rule) match(sender, e164, text string, links []string) bool {
	switch r.Kind {
	case KindSender:
		return r.re.MatchString(sender)
	case KindDestination:
		return r.re.MatchString(e164)
	case KindContent:
		return r.re.MatchString(text)
	case KindURL:
		for _, l := range links {
			h := strings.ToLower(l)
			for _, p := range []string{"https://", "http://"} {
				h = strings.TrimPrefix(h, p)
			}
			h, _, _ = strings.Cut(h, "/")
			h = strings.TrimPrefix(h, "www.")
			if h == r.Pattern || strings.HasSuffix(h, "."+r.Pattern) {
				return true
			}
		}
	}
	return false
}

var kindReason = map[string]Reason{KindSender: ReasonSender, KindContent: ReasonContent, KindURL: ReasonURL, KindDestination: ReasonDestination}

// Limits are an account's velocity limits; 0 turns one off.
type Limits struct {
	DestPerHour     int // messages to one number per hour
	SeriesPer10Min  int // messages into one 1,000-number block per 10 minutes
	SeriesBlockMins int // how long a block stays shut after tripping
}

// DefaultLimits apply to accounts without their own row.
var DefaultLimits = Limits{DestPerHour: 50, SeriesPer10Min: 300, SeriesBlockMins: 60}

// Firewall checks messages.
type Firewall struct {
	PG    *pgxpool.Pool
	Redis redis.UniversalClient
	Log   *slog.Logger

	rules  atomic.Pointer[[]*Rule]
	mu     sync.RWMutex
	limits map[string]cachedLimits
}

type cachedLimits struct {
	l       Limits
	expires time.Time
}

// Load reads the enabled rules.
func (f *Firewall) Load(ctx context.Context) error {
	rows, err := f.PG.Query(ctx, `SELECT id, coalesce(account_id::text, ''), kind, pattern, action
		FROM firewall_rules WHERE enabled ORDER BY id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var rules []*Rule
	for rows.Next() {
		r := &Rule{}
		if err := rows.Scan(&r.ID, &r.AccountID, &r.Kind, &r.Pattern, &r.Action); err != nil {
			return err
		}
		if err := r.Compile(); err != nil {
			if f.Log != nil {
				f.Log.Warn("firewall rule skipped", "rule", r.ID, "err", err)
			}
			continue
		}
		rules = append(rules, r)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	f.rules.Store(&rules)
	return nil
}

// Run reloads rules every interval, keeping the last good set on errors.
func (f *Firewall) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := f.Load(ctx); err != nil && f.Log != nil {
				f.Log.Warn("firewall rules not reloaded", "err", err)
			}
		}
	}
}

// CheckRules applies the block lists. Flag rules only count.
func (f *Firewall) CheckRules(accountID, sender, e164, text string) *Error {
	rp := f.rules.Load()
	if rp == nil {
		return nil
	}
	var links []string
	linksDone := false
	for _, r := range *rp {
		if r.AccountID != "" && r.AccountID != accountID {
			continue
		}
		if r.Kind == KindURL && !linksDone {
			links, linksDone = dlt.Links(text), true
		}
		if !r.match(sender, e164, text, links) {
			continue
		}
		metrics.FirewallHits.WithLabelValues(r.Kind, r.Action).Inc()
		if r.Action == "reject" {
			return &Error{Reason: kindReason[r.Kind], Detail: fmt.Sprintf("blocked by firewall rule %d", r.ID)}
		}
	}
	return nil
}

//go:embed velocity.lua
var velocitySrc string
var velocityScript = redis.NewScript(velocitySrc)

// Check applies the rules, then the velocity limits, in its own Redis
// call. A refusal is an *Error; any other error is infrastructure.
func (f *Firewall) Check(ctx context.Context, accountID, sender, e164, text string) error {
	v, err := f.Prepare(ctx, accountID, sender, e164, text)
	if err != nil || v == nil {
		return err
	}
	_, err = f.Redis.Pipelined(ctx, func(p redis.Pipeliner) error { v.Queue(ctx, p); return nil })
	if err != nil && !isNoScript(err) {
		return fmt.Errorf("firewall: %w", err)
	}
	return v.Result(ctx, f.Redis)
}

// Velocity is a prepared velocity check, so the caller can send it in the
// same pipeline as its other Redis work.
type Velocity struct {
	f       *Firewall
	account string
	series  string
	keys    []string
	args    []any
	limits  Limits
	cmd     *redis.Cmd
}

// Prepare applies the rules and returns the velocity check to run, or nil
// when the account has no velocity limits.
func (f *Firewall) Prepare(ctx context.Context, accountID, sender, e164, text string) (*Velocity, error) {
	if e := f.CheckRules(accountID, sender, e164, text); e != nil {
		return nil, e
	}
	if f.Redis == nil || len(e164) < 9 {
		return nil, nil
	}
	l, err := f.limitsFor(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if l.DestPerHour == 0 && l.SeriesPer10Min == 0 {
		return nil, nil
	}
	series := Series(e164)
	now := time.Now().UTC()
	return &Velocity{f: f, account: accountID, series: series, limits: l,
		keys: []string{blockKey(accountID, series), hourKey(accountID, series, now)},
		args: []any{e164[len(series):], fmt.Sprintf("s%d", now.Minute()/10), l.DestPerHour, l.SeriesPer10Min, l.SeriesBlockMins * 60},
	}, nil
}

func blockKey(acct, series string) string { return fmt.Sprintf("fw:{%s}:b:%s", acct, series) }
func hourKey(acct, series string, t time.Time) string {
	return fmt.Sprintf("fw:{%s}:h:%s:%s", acct, series, t.Format("2006010215"))
}

// Queue adds the check to a pipeline.
func (v *Velocity) Queue(ctx context.Context, p redis.Pipeliner) {
	v.cmd = velocityScript.EvalSha(ctx, p, v.keys, v.args...)
}

func isNoScript(err error) bool { return err != nil && strings.HasPrefix(err.Error(), "NOSCRIPT") }

// Result reads the check after the pipeline ran. If Redis had lost the
// script (restart), it is run again directly.
func (v *Velocity) Result(ctx context.Context, rdb redis.UniversalClient) error {
	res, err := v.cmd.Int()
	if isNoScript(err) {
		res, err = velocityScript.Run(ctx, rdb, v.keys, v.args...).Int()
	}
	if err != nil {
		return fmt.Errorf("firewall: %w", err)
	}
	l := v.limits
	switch res {
	case 1:
		metrics.FirewallHits.WithLabelValues("destination_rate", "reject").Inc()
		return &Error{Reason: ReasonDestRate, Detail: fmt.Sprintf("more than %d messages to this number in an hour", l.DestPerHour)}
	case 2:
		metrics.FirewallHits.WithLabelValues("series_tripped", "reject").Inc()
		if v.f.Log != nil {
			v.f.Log.Warn("number range blocked for suspected SMS pumping", "account", v.account, "range", v.series+"xxx",
				"limit_per_10min", l.SeriesPer10Min, "block_minutes", l.SeriesBlockMins)
		}
		return &Error{Reason: ReasonSeries, Detail: fmt.Sprintf("number range %sxxx blocked for %d minutes: unusual volume (suspected SMS pumping)", v.series, l.SeriesBlockMins)}
	case 3:
		metrics.FirewallHits.WithLabelValues("series_blocked", "reject").Inc()
		return &Error{Reason: ReasonSeries, Detail: fmt.Sprintf("number range %sxxx is blocked (suspected SMS pumping)", v.series)}
	}
	return nil
}

// Series is a number's 1,000-number block: the E.164 number without its
// last three digits.
func Series(e164 string) string { return e164[:len(e164)-3] }

func (f *Firewall) limitsFor(ctx context.Context, accountID string) (Limits, error) {
	now := time.Now()
	f.mu.RLock()
	c, ok := f.limits[accountID]
	f.mu.RUnlock()
	if ok && now.Before(c.expires) {
		return c.l, nil
	}
	l := DefaultLimits
	err := f.PG.QueryRow(ctx, `SELECT dest_per_hour, series_per_10min, series_block_minutes FROM firewall_limits WHERE account_id = $1`,
		accountID).Scan(&l.DestPerHour, &l.SeriesPer10Min, &l.SeriesBlockMins)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return l, fmt.Errorf("firewall: %w", err)
	}
	f.mu.Lock()
	if f.limits == nil {
		f.limits = map[string]cachedLimits{}
	}
	f.limits[accountID] = cachedLimits{l: l, expires: now.Add(time.Minute)}
	f.mu.Unlock()
	return l, nil
}

// Blocked lists an account's blocked number ranges and seconds left.
func Blocked(ctx context.Context, rdb redis.UniversalClient, accountID string) (map[string]int64, error) {
	out := map[string]int64{}
	prefix := fmt.Sprintf("fw:{%s}:b:", accountID)
	iter := rdb.Scan(ctx, 0, prefix+"*", 1000).Iterator()
	for iter.Next(ctx) {
		k := iter.Val()
		if ttl, err := rdb.TTL(ctx, k).Result(); err == nil {
			out[strings.TrimPrefix(k, prefix)+"xxx"] = int64(ttl.Seconds())
		}
	}
	return out, iter.Err()
}

// Unblock reopens a number range for an account and resets its count for
// the current 10 minutes, so it does not trip again straight away (an
// operator unblocks after deciding the traffic is genuine).
func Unblock(ctx context.Context, rdb redis.UniversalClient, accountID, series string) (bool, error) {
	series = strings.TrimSuffix(series, "xxx")
	n, err := rdb.Del(ctx, blockKey(accountID, series)).Result()
	if err != nil || n == 0 {
		return false, err
	}
	now := time.Now().UTC()
	err = rdb.HDel(ctx, hourKey(accountID, series, now), fmt.Sprintf("s%d", now.Minute()/10)).Err()
	return true, err
}
