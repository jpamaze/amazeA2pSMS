package dlt

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jpamaze/amazeA2pSMS/internal/domain"
	"github.com/jpamaze/amazeA2pSMS/internal/numbering"
)

// Categories.
const (
	Transactional   = "transactional"
	ServiceImplicit = "service_implicit"
	ServiceExplicit = "service_explicit"
	Promotional     = "promotional"
)

// Account is one account's DLT registrations.
type Account struct {
	Mode     string                 // enforce | off
	byPE     map[string]*entity     // PE ID -> entity
	byHeader map[string][]*header   // upper-case header -> headers (normally one)
	byTplID  map[string]*template   // DLT template ID -> template
	tplsOf   map[string][]*template // header row ID -> its templates
}

type entity struct {
	id, peID, status, chain string
}

type header struct {
	id, text, category, status string
	ent                        *entity
}

type template struct {
	id, templateID, body, category, status string
	consent                                bool
	ent                                    *entity
	headers                                map[string]bool // header row IDs
	wl                                     Whitelist
	m                                      *Compiled // set when loaded; nil if the body cannot compile
	merr                                   error
}

// matcher returns the template compiled at load; templates built any
// other way (tests) compile through the shared cache, never writing to the
// template, which concurrent checks share.
func (t *template) matcher() (*Compiled, error) {
	if t.m != nil || t.merr != nil {
		return t.m, t.merr
	}
	return compileCached(t.body)
}

// Checker checks messages against cached DLT registrations.
type Checker struct {
	PG           *pgxpool.Pool
	TTL          time.Duration    // cache per account (default 1 minute; negative = no cache)
	RequireChain bool             // refuse messages without a TM chain
	Now          func() time.Time // for tests

	mu    sync.RWMutex
	cache map[string]cachedAccount
}

type cachedAccount struct {
	acct    *Account
	expires time.Time
}

// IST is India Standard Time (no daylight saving).
var IST = time.FixedZone("IST", 5*3600+1800)

// Promotional hours, IST.
const (
	PromoStartHour = 9
	PromoEndHour   = 21
)

// Check validates one message and returns its DLT fields completed with the
// entity's PE ID, the matched template's ID, its category and the TM chain.
// partial is true for one part of a message the customer split itself
// (UDH): the text cannot be matched, so a template ID is required instead.
// A refusal is an *Error; any other error is infrastructure.
func (c *Checker) Check(ctx context.Context, accountID, sender, text string, partial bool, f domain.DLTFields) (domain.DLTFields, error) {
	a, err := c.account(ctx, accountID)
	if err != nil {
		return f, err
	}
	return c.check(a, sender, text, partial, f)
}

func (c *Checker) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Checker) check(a *Account, sender, text string, partial bool, f domain.DLTFields) (domain.DLTFields, error) {
	if a.Mode == "off" {
		return f, nil
	}
	// Entity: given, or found from the header.
	var ent *entity
	if f.PEID != "" {
		ent = a.byPE[f.PEID]
		if ent == nil {
			return f, fail(ReasonEntity, "PE ID %s is not registered for this account", f.PEID)
		}
		if ent.status != "active" {
			return f, fail(ReasonEntity, "PE ID %s is suspended", f.PEID)
		}
	}

	// Header.
	var h *header
	for _, cand := range a.byHeader[strings.ToUpper(sender)] {
		if ent == nil || cand.ent == ent {
			h = cand
			break
		}
	}
	if h == nil {
		if ent != nil {
			return f, fail(ReasonHeader, "header %s is not registered under PE ID %s", sender, ent.peID)
		}
		return f, fail(ReasonHeader, "header %s is not registered", sender)
	}
	if h.status != "active" {
		return f, fail(ReasonHeader, "header %s is suspended", sender)
	}
	if ent == nil {
		ent = h.ent
		if ent.status != "active" {
			return f, fail(ReasonEntity, "PE ID %s is suspended", ent.peID)
		}
	}

	// Template: the one named, or the most specific one that matches.
	var t *template
	var vals []Value
	if f.TemplateID != "" {
		t = a.byTplID[f.TemplateID]
		if t == nil || t.ent != ent || !t.headers[h.id] {
			return f, fail(ReasonTemplate, "template %s is not registered for header %s", f.TemplateID, sender)
		}
		if t.status != "active" {
			return f, fail(ReasonTemplate, "template %s is suspended", f.TemplateID)
		}
		if !partial {
			m, err := t.matcher()
			if err != nil {
				return f, fail(ReasonTemplate, "template %s cannot be matched: %v", f.TemplateID, err)
			}
			var ok bool
			if vals, ok = m.Match(text); !ok {
				return f, fail(ReasonTemplate, "text does not match template %s", f.TemplateID)
			}
		}
	} else {
		if partial {
			return f, fail(ReasonTemplate, "a template ID is required for messages split by the sender (UDH)")
		}
		best := -1
		for _, cand := range a.tplsOf[h.id] {
			if cand.status != "active" {
				continue
			}
			m, err := cand.matcher()
			if err != nil || m.lit <= best {
				continue
			}
			if v, ok := m.Match(text); ok {
				t, vals, best = cand, v, m.lit
			}
		}
		if t == nil {
			return f, fail(ReasonTemplate, "text matches no active template registered for header %s", sender)
		}
	}

	// Category against the header type, promotional hours, consent.
	if (t.category == Promotional) != (h.category == Promotional) {
		return f, fail(ReasonCategory, "%s template %s cannot be sent from %s header %s", t.category, t.templateID, h.category, sender)
	}
	if t.category == Promotional {
		hr := c.now().In(IST).Hour()
		if hr < PromoStartHour || hr >= PromoEndHour {
			return f, fail(ReasonPromoHours, "promotional template %s outside 09:00-21:00 IST", t.templateID)
		}
	}
	if t.category == ServiceExplicit && !t.consent {
		return f, fail(ReasonConsent, "service explicit template %s has no consent template", t.templateID)
	}

	if e := t.wl.checkValues(vals); e != nil {
		return f, e
	}

	chain := f.TMChain
	if chain == "" {
		chain = ent.chain
	}
	if chain == "" && c.RequireChain {
		return f, fail(ReasonChain, "no telemarketer chain registered for PE ID %s", ent.peID)
	}
	return domain.DLTFields{PEID: ent.peID, TemplateID: t.templateID, TMChain: chain, Category: t.category}, nil
}

func (c *Checker) account(ctx context.Context, id string) (*Account, error) {
	ttl := c.TTL
	if ttl == 0 {
		ttl = time.Minute
	}
	now := time.Now()
	if ttl > 0 {
		c.mu.RLock()
		ca, ok := c.cache[id]
		c.mu.RUnlock()
		if ok && now.Before(ca.expires) {
			return ca.acct, nil
		}
	}
	a, err := Load(ctx, c.PG, id)
	if err != nil {
		return nil, err
	}
	if ttl > 0 {
		c.mu.Lock()
		if c.cache == nil {
			c.cache = map[string]cachedAccount{}
		}
		c.cache[id] = cachedAccount{acct: a, expires: now.Add(ttl)}
		c.mu.Unlock()
	}
	return a, nil
}

// Load reads an account's DLT registrations in one round trip.
func Load(ctx context.Context, pg *pgxpool.Pool, accountID string) (*Account, error) {
	b := &pgx.Batch{}
	b.Queue(`SELECT dlt_mode FROM accounts WHERE id = $1`, accountID)
	b.Queue(`SELECT e.id, e.pe_id, e.status, coalesce((SELECT c.chain_value FROM dlt_tm_chains c
		WHERE c.entity_id = e.id AND c.valid_from <= now() AND (c.valid_to IS NULL OR c.valid_to > now())
		ORDER BY c.valid_from DESC LIMIT 1), '')
		FROM dlt_entities e WHERE e.account_id = $1`, accountID)
	b.Queue(`SELECT h.id, h.entity_id, h.header, h.category, h.status
		FROM dlt_headers h JOIN dlt_entities e ON e.id = h.entity_id WHERE e.account_id = $1`, accountID)
	b.Queue(`SELECT t.id, t.entity_id, t.template_id, t.body, t.category, t.status, t.consent_template_id IS NOT NULL
		FROM dlt_templates t JOIN dlt_entities e ON e.id = t.entity_id WHERE e.account_id = $1`, accountID)
	b.Queue(`SELECT th.template_id, th.header_id FROM dlt_template_headers th
		JOIN dlt_templates t ON t.id = th.template_id JOIN dlt_entities e ON e.id = t.entity_id WHERE e.account_id = $1`, accountID)
	b.Queue(`SELECT w.template_id, w.kind, w.value FROM dlt_template_whitelist w
		JOIN dlt_templates t ON t.id = w.template_id JOIN dlt_entities e ON e.id = t.entity_id WHERE e.account_id = $1`, accountID)
	br := pg.SendBatch(ctx, b)
	defer br.Close()

	a := &Account{byPE: map[string]*entity{}, byHeader: map[string][]*header{}, byTplID: map[string]*template{}, tplsOf: map[string][]*template{}}
	if err := br.QueryRow().Scan(&a.Mode); err != nil {
		return nil, fmt.Errorf("dlt: account %s: %w", accountID, err)
	}
	ents := map[string]*entity{}
	rows, err := br.Query()
	if err != nil {
		return nil, fmt.Errorf("dlt: %w", err)
	}
	for rows.Next() {
		e := &entity{}
		if err := rows.Scan(&e.id, &e.peID, &e.status, &e.chain); err != nil {
			return nil, fmt.Errorf("dlt: %w", err)
		}
		ents[e.id], a.byPE[e.peID] = e, e
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("dlt: %w", err)
	}
	hdrs := map[string]*header{}
	if rows, err = br.Query(); err != nil {
		return nil, fmt.Errorf("dlt: %w", err)
	}
	for rows.Next() {
		h := &header{}
		var entID string
		if err := rows.Scan(&h.id, &entID, &h.text, &h.category, &h.status); err != nil {
			return nil, fmt.Errorf("dlt: %w", err)
		}
		h.ent = ents[entID]
		hdrs[h.id] = h
		k := strings.ToUpper(h.text)
		a.byHeader[k] = append(a.byHeader[k], h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("dlt: %w", err)
	}
	tpls := map[string]*template{}
	if rows, err = br.Query(); err != nil {
		return nil, fmt.Errorf("dlt: %w", err)
	}
	for rows.Next() {
		t := &template{headers: map[string]bool{}}
		var entID string
		if err := rows.Scan(&t.id, &entID, &t.templateID, &t.body, &t.category, &t.status, &t.consent); err != nil {
			return nil, fmt.Errorf("dlt: %w", err)
		}
		t.ent = ents[entID]
		t.m, t.merr = compileCached(t.body)
		tpls[t.id], a.byTplID[t.templateID] = t, t
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("dlt: %w", err)
	}
	if rows, err = br.Query(); err != nil {
		return nil, fmt.Errorf("dlt: %w", err)
	}
	for rows.Next() {
		var tid, hid string
		if err := rows.Scan(&tid, &hid); err != nil {
			return nil, fmt.Errorf("dlt: %w", err)
		}
		if t := tpls[tid]; t != nil && hdrs[hid] != nil {
			t.headers[hid] = true
			a.tplsOf[hid] = append(a.tplsOf[hid], t)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("dlt: %w", err)
	}
	if rows, err = br.Query(); err != nil {
		return nil, fmt.Errorf("dlt: %w", err)
	}
	for rows.Next() {
		var tid, kind, val string
		if err := rows.Scan(&tid, &kind, &val); err != nil {
			return nil, fmt.Errorf("dlt: %w", err)
		}
		if t := tpls[tid]; t != nil {
			t.wl.Add(kind, val)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("dlt: %w", err)
	}
	return a, nil
}

// HeaderTypeOK reports whether a header's text suits its category:
// promotional headers are numeric, the others alphabetic.
func HeaderTypeOK(text, category string) bool {
	if category == Promotional {
		return numbering.IsNumeric(text) && len(text) == 6
	}
	if len(text) != 6 {
		return false
	}
	for _, r := range text {
		if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z') {
			return false
		}
	}
	return true
}

// NormCategory maps the category names used on operator DLT portals to ours.
func NormCategory(s string) (string, bool) {
	k := strings.Join(strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return r == ' ' || r == '-' || r == '_' }), "")
	switch k {
	case "transactional", "txn", "trans":
		return Transactional, true
	case "serviceimplicit", "si", "implicit":
		return ServiceImplicit, true
	case "serviceexplicit", "se", "explicit":
		return ServiceExplicit, true
	case "promotional", "promo":
		return Promotional, true
	}
	return "", false
}
