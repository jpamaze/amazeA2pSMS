package adminapi

import (
	"encoding/csv"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/jpamaze/amazeA2pSMS/internal/firewall"
	"github.com/jpamaze/amazeA2pSMS/internal/network"
	"github.com/jpamaze/amazeA2pSMS/internal/numbering"
	"github.com/jpamaze/amazeA2pSMS/internal/vendorhealth"
)

// networkRoutes registers routing, failover, HLR/MNP and firewall
// administration (phase 3 slice 2). Everything here is platform-only except
// reading an account's blocked number ranges.
func (a *API) networkRoutes(mux *http.ServeMux) {
	mux.Handle("GET /v1/routes", a.authed(PermRead, a.listRoutes))
	mux.Handle("POST /v1/routes", a.authed(PermAdmin, a.createRoute))
	mux.Handle("DELETE /v1/routes/{route}", a.authed(PermAdmin, a.disableRoute))
	mux.Handle("GET /v1/vendors/health", a.authed(PermRead, a.vendorHealth))
	mux.Handle("PUT /v1/vendors/{vendor}/failover", a.authed(PermAdmin, a.setFailover))
	mux.Handle("POST /v1/number-series/import", a.authed(PermAdmin, a.importSeries))
	mux.Handle("POST /v1/mnp/import", a.authed(PermAdmin, a.importMNP))
	mux.Handle("GET /v1/numbers/{msisdn}", a.authed(PermRead, a.numberInfo))
	mux.Handle("GET /v1/firewall/rules", a.authed(PermRead, a.listFirewallRules))
	mux.Handle("POST /v1/firewall/rules", a.authed(PermAdmin, a.createFirewallRule))
	mux.Handle("DELETE /v1/firewall/rules/{rule}", a.authed(PermAdmin, a.disableFirewallRule))
	mux.Handle("PUT /v1/accounts/{id}/firewall-limits", a.authed(PermAdmin, a.setFirewallLimits))
	mux.Handle("GET /v1/accounts/{id}/firewall/blocks", a.authed(PermRead, a.listBlocks))
	mux.Handle("DELETE /v1/accounts/{id}/firewall/blocks/{range}", a.authed(PermAdmin, a.unblockRange))
}

func (a *API) listRoutes(w http.ResponseWriter, r *http.Request, act actor) {
	if !platformOnly(w, act) {
		return
	}
	rows, err := a.PG.Query(r.Context(), `SELECT id, coalesce(account_id::text, ''), mcc, mnc, mode, coalesce(vendor_id, ''),
		priority, weight, hlr, hlr_reject, enabled FROM routes ORDER BY account_id NULLS FIRST, mcc, priority`)
	if err != nil {
		writeDBErr(w, err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, acct, mcc, mnc, mode, vendor string
		var prio, weight int
		var hlr, rej, en bool
		if err := rows.Scan(&id, &acct, &mcc, &mnc, &mode, &vendor, &prio, &weight, &hlr, &rej, &en); err != nil {
			writeDBErr(w, err)
			return
		}
		out = append(out, map[string]any{"id": id, "account_id": acct, "mcc": mcc, "mnc": mnc, "mode": mode, "vendor_id": vendor,
			"priority": prio, "weight": weight, "hlr": hlr, "hlr_reject": rej, "enabled": en})
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) createRoute(w http.ResponseWriter, r *http.Request, act actor) {
	if !platformOnly(w, act) {
		return
	}
	var req struct {
		AccountID string `json:"account_id"` // empty = platform default
		MCC       string `json:"mcc"`
		MNC       string `json:"mnc"`
		Mode      string `json:"mode"` // lcr | priority | weighted
		VendorID  string `json:"vendor_id"`
		Priority  int    `json:"priority"`
		Weight    int    `json:"weight"`
		HLR       bool   `json:"hlr"`
		HLRReject bool   `json:"hlr_reject"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.MNC == "" {
		req.MNC = "*"
	}
	if req.Priority == 0 {
		req.Priority = 100
	}
	if req.Weight == 0 {
		req.Weight = 1
	}
	if err := validDest(req.MCC, req.MNC); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if (req.Mode == "lcr") != (req.VendorID == "") || (req.Mode != "lcr" && req.Mode != "priority" && req.Mode != "weighted") {
		writeErr(w, http.StatusBadRequest, "mode lcr (no vendor_id), or priority/weighted with a vendor_id")
		return
	}
	if req.HLRReject && !req.HLR {
		writeErr(w, http.StatusBadRequest, "hlr_reject needs hlr")
		return
	}
	var acct, vendor *string
	if req.AccountID != "" {
		acct = &req.AccountID
	}
	if req.VendorID != "" {
		vendor = &req.VendorID
	}
	var id string
	err := a.PG.QueryRow(r.Context(), `INSERT INTO routes (account_id, mcc, mnc, mode, vendor_id, priority, weight, hlr, hlr_reject)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING id`,
		acct, req.MCC, req.MNC, req.Mode, vendor, req.Priority, req.Weight, req.HLR, req.HLRReject).Scan(&id)
	if err != nil {
		writeDBErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": id, "note": "routers apply it within 15 seconds"})
}

func (a *API) disableRoute(w http.ResponseWriter, r *http.Request, act actor) {
	if !platformOnly(w, act) {
		return
	}
	tag, err := a.PG.Exec(r.Context(), `UPDATE routes SET enabled = false WHERE id = $1 AND enabled`, r.PathValue("route"))
	if err != nil {
		writeDBErr(w, err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeErr(w, http.StatusNotFound, "no enabled route with this id")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "disabled"})
}

// vendorHealth shows each vendor's failover verdict, its policy and what
// its connectors report.
func (a *API) vendorHealth(w http.ResponseWriter, r *http.Request, act actor) {
	if !platformOnly(w, act) {
		return
	}
	if a.Redis == nil {
		writeErr(w, http.StatusServiceUnavailable, "needs Redis")
		return
	}
	states, err := vendorhealth.States(r.Context(), a.Redis)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "health store unavailable")
		return
	}
	reports, _ := vendorhealth.Reports(r.Context(), a.Redis)
	rows, err := a.PG.Query(r.Context(), `SELECT id, status, failover_enabled, failover_max_submit_ms, failover_max_error_pct,
		failover_min_dlr_pct, failover_dlr_window_min, failover_cooldown_s FROM vendors ORDER BY id`)
	if err != nil {
		writeDBErr(w, err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, status string
		var en bool
		var ms, errPct, dlrPct, win, cool int
		if err := rows.Scan(&id, &status, &en, &ms, &errPct, &dlrPct, &win, &cool); err != nil {
			writeDBErr(w, err)
			return
		}
		out = append(out, map[string]any{"vendor_id": id, "status": status, "routing": states[id], "connectors": reports[id],
			"policy": map[string]any{"enabled": en, "max_submit_ms": ms, "max_error_pct": errPct, "min_dlr_pct": dlrPct,
				"dlr_window_min": win, "cooldown_s": cool}})
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) setFailover(w http.ResponseWriter, r *http.Request, act actor) {
	if !platformOnly(w, act) {
		return
	}
	var req struct {
		Enabled      bool `json:"enabled"`
		MaxSubmitMs  int  `json:"max_submit_ms"`
		MaxErrorPct  int  `json:"max_error_pct"`
		MinDLRPct    int  `json:"min_dlr_pct"`
		DLRWindowMin int  `json:"dlr_window_min"`
		CooldownS    int  `json:"cooldown_s"`
	}
	req.DLRWindowMin, req.CooldownS = 15, 30
	if !decode(w, r, &req) {
		return
	}
	tag, err := a.PG.Exec(r.Context(), `UPDATE vendors SET failover_enabled = $2, failover_max_submit_ms = $3,
		failover_max_error_pct = $4, failover_min_dlr_pct = $5, failover_dlr_window_min = $6, failover_cooldown_s = $7
		WHERE id = $1`, r.PathValue("vendor"), req.Enabled, req.MaxSubmitMs, req.MaxErrorPct, req.MinDLRPct, req.DLRWindowMin, req.CooldownS)
	if err != nil {
		writeDBErr(w, err) // CHECK violations come back as 400
		return
	}
	if tag.RowsAffected() == 0 {
		writeErr(w, http.StatusNotFound, "no such vendor")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "saved", "note": "routers apply it within 15 seconds"})
}

// importSeries reads prefix,mcc,mnc[,operator,circle] (national prefixes);
// ?replace=true replaces the whole table.
func (a *API) importSeries(w http.ResponseWriter, r *http.Request, act actor) {
	if !platformOnly(w, act) {
		return
	}
	cr := csv.NewReader(io.LimitReader(r.Body, 16<<20))
	cr.FieldsPerRecord = -1
	cr.TrimLeadingSpace = true
	recs, err := cr.ReadAll()
	if err != nil {
		writeErr(w, http.StatusBadRequest, "CSV: "+err.Error())
		return
	}
	if len(recs) > 0 && strings.EqualFold(strings.TrimSpace(recs[0][0]), "prefix") {
		recs = recs[1:]
	}
	var bad []string
	for i, rec := range recs {
		if len(rec) < 3 || !numbering.IsNumeric(rec[0]) || len(rec[0]) < 2 || len(rec[0]) > 9 || !network.ValidMCCMNC(rec[1], rec[2]) {
			bad = append(bad, fmt.Sprintf("line %d: want prefix (2-9 digits, national),mcc,mnc[,operator,circle]", i+2))
		}
	}
	if len(bad) > 0 || len(recs) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "nothing imported", "lines": bad})
		return
	}
	err = pgx.BeginFunc(r.Context(), a.PG, func(tx pgx.Tx) error {
		if r.URL.Query().Get("replace") == "true" {
			if _, err := tx.Exec(r.Context(), `DELETE FROM number_series`); err != nil {
				return err
			}
		}
		for _, rec := range recs {
			op, circle := "", ""
			if len(rec) > 3 {
				op = rec[3]
			}
			if len(rec) > 4 {
				circle = rec[4]
			}
			if _, err := tx.Exec(r.Context(), `INSERT INTO number_series (prefix, mcc, mnc, operator, circle) VALUES ($1, $2, $3, $4, $5)
				ON CONFLICT (prefix) DO UPDATE SET mcc = EXCLUDED.mcc, mnc = EXCLUDED.mnc, operator = EXCLUDED.operator, circle = EXCLUDED.circle`,
				rec[0], rec[1], rec[2], op, circle); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		writeDBErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"imported": len(recs), "note": "edges reload number series within 5 minutes"})
}

// importMNP streams msisdn,mcc,mnc into the MNP store (Redis). An empty mnc
// removes a number (ported back). Bad lines are skipped and reported.
func (a *API) importMNP(w http.ResponseWriter, r *http.Request, act actor) {
	if !platformOnly(w, act) {
		return
	}
	if a.Redis == nil {
		writeErr(w, http.StatusServiceUnavailable, "needs Redis")
		return
	}
	n, bad, err := network.ImportMNP(r.Context(), a.Redis, io.LimitReader(r.Body, 8<<30))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error(), "applied_before_error": n, "lines": bad})
		return
	}
	_, _ = a.PG.Exec(r.Context(), `INSERT INTO mnp_imports (rows, bad_lines, imported_by) VALUES ($1, $2, $3)`, n, len(bad), act.User)
	writeJSON(w, http.StatusOK, map[string]any{"applied": n, "bad_lines": bad, "note": "effective immediately"})
}

// numberInfo shows how a number resolves: series, MNP, cached HLR answer.
func (a *API) numberInfo(w http.ResponseWriter, r *http.Request, act actor) {
	if !platformOnly(w, act) {
		return
	}
	d, err := numbering.NormalizeIndia(r.PathValue("msisdn"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "not an Indian mobile number")
		return
	}
	series := &network.Series{PG: a.PG}
	if err := series.Load(r.Context()); err != nil {
		writeDBErr(w, err)
		return
	}
	out := map[string]any{"e164": d.E164}
	if n, ok := series.Lookup(strings.TrimPrefix(d.E164, "91")); ok {
		out["series"] = n
	}
	res := &network.Resolver{Series: series, Redis: a.Redis}
	if n, err := res.Resolve(r.Context(), d.E164); err == nil {
		out["resolved"] = n // what ingest uses
	}
	if a.Redis != nil {
		h := &network.HLR{Redis: a.Redis}
		if c, err := h.Cached(r.Context(), []string{d.E164}); err == nil {
			if v, ok := c[d.E164]; ok {
				out["hlr_cached"] = v
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) listFirewallRules(w http.ResponseWriter, r *http.Request, act actor) {
	if !platformOnly(w, act) {
		return
	}
	rows, err := a.PG.Query(r.Context(), `SELECT id, coalesce(account_id::text, ''), kind, pattern, action, note, enabled, created_by
		FROM firewall_rules ORDER BY id`)
	if err != nil {
		writeDBErr(w, err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id int64
		var acct, kind, pat, action, note, by string
		var en bool
		if err := rows.Scan(&id, &acct, &kind, &pat, &action, &note, &en, &by); err != nil {
			writeDBErr(w, err)
			return
		}
		out = append(out, map[string]any{"id": id, "account_id": acct, "kind": kind, "pattern": pat, "action": action,
			"note": note, "enabled": en, "created_by": by})
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) createFirewallRule(w http.ResponseWriter, r *http.Request, act actor) {
	if !platformOnly(w, act) {
		return
	}
	var req struct {
		AccountID string `json:"account_id"` // empty = every account
		Kind      string `json:"kind"`
		Pattern   string `json:"pattern"`
		Action    string `json:"action"`
		Note      string `json:"note"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Action == "" {
		req.Action = "reject"
	}
	rule := firewall.Rule{Kind: req.Kind, Pattern: req.Pattern, Action: req.Action}
	if err := rule.Compile(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var acct *string
	if req.AccountID != "" {
		acct = &req.AccountID
	}
	var id int64
	if err := a.PG.QueryRow(r.Context(), `INSERT INTO firewall_rules (account_id, kind, pattern, action, note, created_by)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`, acct, req.Kind, rule.Pattern, req.Action, req.Note, act.User).Scan(&id); err != nil {
		writeDBErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "note": "edges apply it within 30 seconds"})
}

func (a *API) disableFirewallRule(w http.ResponseWriter, r *http.Request, act actor) {
	if !platformOnly(w, act) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("rule"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "rule id is a number")
		return
	}
	tag, err := a.PG.Exec(r.Context(), `UPDATE firewall_rules SET enabled = false WHERE id = $1 AND enabled`, id)
	if err != nil {
		writeDBErr(w, err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeErr(w, http.StatusNotFound, "no enabled rule with this id")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "disabled"})
}

func (a *API) setFirewallLimits(w http.ResponseWriter, r *http.Request, act actor) {
	if !platformOnly(w, act) {
		return
	}
	req := firewall.DefaultLimits
	var body struct {
		DestPerHour     *int `json:"dest_per_hour"`
		SeriesPer10Min  *int `json:"series_per_10min"`
		SeriesBlockMins *int `json:"series_block_minutes"`
	}
	if !decode(w, r, &body) {
		return
	}
	for _, p := range []struct {
		v   *int
		dst *int
	}{{body.DestPerHour, &req.DestPerHour}, {body.SeriesPer10Min, &req.SeriesPer10Min}, {body.SeriesBlockMins, &req.SeriesBlockMins}} {
		if p.v != nil {
			*p.dst = *p.v
		}
	}
	_, err := a.PG.Exec(r.Context(), `INSERT INTO firewall_limits (account_id, dest_per_hour, series_per_10min, series_block_minutes, updated_by)
		VALUES ($1, $2, $3, $4, $5) ON CONFLICT (account_id) DO UPDATE SET dest_per_hour = EXCLUDED.dest_per_hour,
		series_per_10min = EXCLUDED.series_per_10min, series_block_minutes = EXCLUDED.series_block_minutes,
		updated_by = EXCLUDED.updated_by, updated_at = now()`,
		r.PathValue("id"), req.DestPerHour, req.SeriesPer10Min, req.SeriesBlockMins, act.User)
	if err != nil {
		writeDBErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"dest_per_hour": req.DestPerHour, "series_per_10min": req.SeriesPer10Min,
		"series_block_minutes": req.SeriesBlockMins, "note": "edges apply it within one minute"})
}

func (a *API) listBlocks(w http.ResponseWriter, r *http.Request, act actor) {
	acct, ok := a.managedOrSelf(w, r, act)
	if !ok {
		return
	}
	if a.Redis == nil {
		writeErr(w, http.StatusServiceUnavailable, "needs Redis")
		return
	}
	b, err := firewall.Blocked(r.Context(), a.Redis, acct)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "firewall store unavailable")
		return
	}
	out := []map[string]any{}
	for k, ttl := range b {
		out = append(out, map[string]any{"range": k, "seconds_left": ttl})
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) unblockRange(w http.ResponseWriter, r *http.Request, act actor) {
	if !platformOnly(w, act) {
		return
	}
	if a.Redis == nil {
		writeErr(w, http.StatusServiceUnavailable, "needs Redis")
		return
	}
	ok, err := firewall.Unblock(r.Context(), a.Redis, r.PathValue("id"), r.PathValue("range"))
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "firewall store unavailable")
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "range is not blocked")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "unblocked"})
}
