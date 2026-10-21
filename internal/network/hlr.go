package network

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// HLR statuses.
const (
	HLROK      = "ok"      // reachable subscriber
	HLRAbsent  = "absent"  // valid number, handset off or out of coverage for long
	HLRInvalid = "invalid" // number not in service
	HLRUnknown = "unknown" // provider could not tell
)

// HLRConfig describes a provider's HTTP API. It is generic so any HLR
// provider can be used without code: a GET URL with {msisdn}, an auth
// header, and where the answer's fields are in the JSON response.
type HLRConfig struct {
	URL           string   // e.g. https://hlr.example.com/v1/lookup?number={msisdn}
	AuthHeader    string   // e.g. Authorization
	AuthValue     string   // e.g. Bearer xyz
	MCCPath       string   // dot path, e.g. result.mcc
	MNCPath       string   // e.g. result.mnc
	MCCMNCPath    string   // instead of MCC/MNC paths: "40445" style
	StatusPath    string   // e.g. result.status
	StatusOK      []string // values meaning ok
	StatusAbsent  []string
	StatusInvalid []string
	Timeout       time.Duration
	CacheTTL      time.Duration // default 48h
}

// HLRResult is a lookup answer.
type HLRResult struct {
	MCC    string `json:"mcc,omitempty"`
	MNC    string `json:"mnc,omitempty"`
	Status string `json:"status"`
	At     int64  `json:"at"`
	Cached bool   `json:"-"`
}

// HLR looks numbers up through a provider, with a Redis cache.
type HLR struct {
	Cfg    HLRConfig
	Client *http.Client
	Redis  redis.UniversalClient
}

func hlrKey(e164 string) string { return "hlr:" + e164 }

// Cached returns cached answers for many numbers in one round trip.
func (h *HLR) Cached(ctx context.Context, numbers []string) (map[string]HLRResult, error) {
	out := map[string]HLRResult{}
	if len(numbers) == 0 {
		return out, nil
	}
	keys := make([]string, len(numbers))
	for i, n := range numbers {
		keys[i] = hlrKey(n)
	}
	vals, err := h.Redis.MGet(ctx, keys...).Result()
	if err != nil {
		return out, err
	}
	for i, v := range vals {
		s, ok := v.(string)
		if !ok {
			continue
		}
		var r HLRResult
		if json.Unmarshal([]byte(s), &r) == nil {
			r.Cached = true
			out[numbers[i]] = r
		}
	}
	return out, nil
}

// Lookup asks the provider and caches the answer (also "invalid" and
// "absent" answers; "unknown" ones are not cached).
func (h *HLR) Lookup(ctx context.Context, e164 string) (HLRResult, error) {
	if h.Cfg.URL == "" {
		return HLRResult{}, errors.New("hlr: no provider configured (HLR_URL)")
	}
	timeout := h.Cfg.Timeout
	if timeout == 0 {
		timeout = 2 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.ReplaceAll(h.Cfg.URL, "{msisdn}", url.QueryEscape(e164)), nil)
	if err != nil {
		return HLRResult{}, err
	}
	if h.Cfg.AuthHeader != "" {
		req.Header.Set(h.Cfg.AuthHeader, h.Cfg.AuthValue)
	}
	client := h.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return HLRResult{}, fmt.Errorf("hlr: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return HLRResult{}, fmt.Errorf("hlr: HTTP %d", resp.StatusCode)
	}
	var doc any
	if err := json.Unmarshal(body, &doc); err != nil {
		return HLRResult{}, fmt.Errorf("hlr: response is not JSON")
	}
	r := HLRResult{At: time.Now().Unix(), Status: HLRUnknown}
	if h.Cfg.MCCMNCPath != "" {
		if v := jsonPath(doc, h.Cfg.MCCMNCPath); len(v) >= 5 {
			r.MCC, r.MNC = v[:3], v[3:]
		}
	} else {
		r.MCC, r.MNC = jsonPath(doc, h.Cfg.MCCPath), jsonPath(doc, h.Cfg.MNCPath)
	}
	if !ValidMCCMNC(r.MCC, r.MNC) {
		r.MCC, r.MNC = "", ""
	}
	st := jsonPath(doc, h.Cfg.StatusPath)
	switch {
	case in(st, h.Cfg.StatusOK):
		r.Status = HLROK
	case in(st, h.Cfg.StatusAbsent):
		r.Status = HLRAbsent
	case in(st, h.Cfg.StatusInvalid):
		r.Status = HLRInvalid
	}
	if r.Status != HLRUnknown {
		ttl := h.Cfg.CacheTTL
		if ttl == 0 {
			ttl = 48 * time.Hour
		}
		b, _ := json.Marshal(r)
		_ = h.Redis.Set(ctx, hlrKey(e164), b, ttl).Err()
	}
	return r, nil
}

func in(v string, list []string) bool {
	for _, x := range list {
		if strings.EqualFold(v, x) {
			return true
		}
	}
	return false
}

// jsonPath follows a dot path ("data.0.mnc") and returns the value as text.
func jsonPath(doc any, path string) string {
	if path == "" {
		return ""
	}
	cur := doc
	for _, p := range strings.Split(path, ".") {
		switch v := cur.(type) {
		case map[string]any:
			cur = v[p]
		case []any:
			i, err := strconv.Atoi(p)
			if err != nil || i < 0 || i >= len(v) {
				return ""
			}
			cur = v[i]
		default:
			return ""
		}
	}
	switch v := cur.(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(v)
	}
	return ""
}
