package adminapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/jpamaze/amazeA2pSMS/internal/connector"
	"github.com/jpamaze/amazeA2pSMS/internal/firewall"
	"github.com/jpamaze/amazeA2pSMS/internal/vendorhealth"
)

// opsRoutes registers the reads and vendor onboarding behind the platform
// operations screens (phase 4 slice 2). Platform only, except the DLT header
// list and the firewall limits of an account the actor may see.
func (a *API) opsRoutes(mux *http.ServeMux) {
	mux.Handle("GET /v1/vendors", a.authed(PermRead, a.listVendors))
	mux.Handle("POST /v1/vendors", a.authed(PermAdmin, a.createVendor))
	mux.Handle("GET /v1/vendors/{vendor}", a.authed(PermRead, a.getVendor))
	mux.Handle("PUT /v1/vendors/{vendor}/status", a.authed(PermAdmin, a.setVendorStatus))
	mux.Handle("POST /v1/vendors/{vendor}/binds", a.authed(PermAdmin, a.addBind))
	mux.Handle("DELETE /v1/vendors/{vendor}/binds/{bind}", a.authed(PermAdmin, a.disableBind))
	mux.Handle("PUT /v1/vendors/{vendor}/http-profile", a.authed(PermAdmin, a.putHTTPProfile))
	mux.Handle("GET /v1/vendors/{vendor}/costs", a.authed(PermRead, a.listVendorCosts))
	mux.Handle("GET /v1/vendor-invoices", a.authed(PermRead, a.listVendorInvoices))
	mux.Handle("GET /v1/accounts/{id}/firewall-limits", a.authed(PermRead, a.getFirewallLimits))
	mux.Handle("GET /v1/accounts/{id}/dlt/entities/{pe}/headers", a.authed(PermRead, a.listHeaders))
	mux.Handle("GET /v1/number-series", a.authed(PermRead, a.listSeries))
	mux.Handle("GET /v1/mnp/imports", a.authed(PermRead, a.listMNPImports))
}

var vendorIDRe = regexp.MustCompile(`^[a-z0-9-]{2,32}$`)

func (a *API) listVendors(w http.ResponseWriter, r *http.Request, act actor) {
	if !platformOnly(w, act) {
		return
	}
	rows, err := a.PG.Query(r.Context(), `SELECT v.id, v.name, v.protocol, v.status, v.created_at,
		(SELECT count(*) FROM vendor_binds b WHERE b.vendor_id = v.id AND b.enabled),
		EXISTS (SELECT 1 FROM http_vendor_profiles h WHERE h.vendor_id = v.id)
		FROM vendors v ORDER BY v.id`)
	if err != nil {
		writeDBErr(w, err)
		return
	}
	defer rows.Close()
	var states map[string]vendorhealth.State
	if a.Redis != nil {
		states, _ = vendorhealth.States(r.Context(), a.Redis)
	}
	out := []map[string]any{}
	for rows.Next() {
		var id, name, proto, status string
		var created time.Time
		var binds int
		var profile bool
		if err := rows.Scan(&id, &name, &proto, &status, &created, &binds, &profile); err != nil {
			writeDBErr(w, err)
			return
		}
		v := map[string]any{"id": id, "name": name, "protocol": proto, "status": status, "created_at": created,
			"binds": binds, "http_profile": profile}
		if st, ok := states[id]; ok {
			v["routing"] = st
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) createVendor(w http.ResponseWriter, r *http.Request, act actor) {
	if !platformOnly(w, act) {
		return
	}
	var req struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Protocol string `json:"protocol"`
	}
	if !decode(w, r, &req) {
		return
	}
	if !vendorIDRe.MatchString(req.ID) || strings.TrimSpace(req.Name) == "" || (req.Protocol != "smpp" && req.Protocol != "http") {
		writeErr(w, http.StatusBadRequest, "id (2-32 of a-z, 0-9, -; it names the vendor's queue), name and protocol smpp|http are required")
		return
	}
	// New vendors start paused: routing skips them until binds or a profile
	// are in place and someone activates them.
	if _, err := a.PG.Exec(r.Context(), `INSERT INTO vendors (id, name, protocol, status) VALUES ($1, $2, $3, 'paused')`,
		req.ID, req.Name, req.Protocol); err != nil {
		writeDBErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": req.ID, "status": "paused",
		"next": "add binds (SMPP) or the HTTP profile, start a connector with VENDOR_ID=" + req.ID + ", import costs, then activate"})
}

func (a *API) getVendor(w http.ResponseWriter, r *http.Request, act actor) {
	if !platformOnly(w, act) {
		return
	}
	id := r.PathValue("vendor")
	var name, proto, status string
	var created time.Time
	err := a.PG.QueryRow(r.Context(), `SELECT name, protocol, status, created_at FROM vendors WHERE id = $1`, id).Scan(&name, &proto, &status, &created)
	if err != nil {
		writeDBErr(w, err)
		return
	}
	out := map[string]any{"id": id, "name": name, "protocol": proto, "status": status, "created_at": created}
	rows, err := a.PG.Query(r.Context(), `SELECT id, host, port, tls, system_id, system_type, smpp_version, bind_type,
		window_size, tps_limit, tlv_profile, enabled FROM vendor_binds WHERE vendor_id = $1 ORDER BY enabled DESC, host, system_id`, id)
	if err != nil {
		writeDBErr(w, err)
		return
	}
	binds := []map[string]any{}
	for rows.Next() {
		var bid, host, sys, stype, ver, btype string
		var port, win, tps int
		var tlsOn, en bool
		var tlv json.RawMessage
		if err := rows.Scan(&bid, &host, &port, &tlsOn, &sys, &stype, &ver, &btype, &win, &tps, &tlv, &en); err != nil {
			rows.Close()
			writeDBErr(w, err)
			return
		}
		binds = append(binds, map[string]any{"id": bid, "host": host, "port": port, "tls": tlsOn, "system_id": sys, "system_type": stype,
			"smpp_version": ver, "bind_type": btype, "window_size": win, "tps_limit": tps, "tlv_profile": tlv, "enabled": en})
	}
	rows.Close()
	out["binds"] = binds
	var base, token string
	var auth, req, resp, dlr json.RawMessage
	var tps, conc int
	var hasSecret bool
	err = a.PG.QueryRow(r.Context(), `SELECT base_url, auth, request_template, response_mapping, dlr_mapping, tps_limit, concurrency,
		dlr_token, secret_enc IS NOT NULL FROM http_vendor_profiles WHERE vendor_id = $1`, id).
		Scan(&base, &auth, &req, &resp, &dlr, &tps, &conc, &token, &hasSecret)
	if err == nil {
		out["http_profile"] = map[string]any{"base_url": base, "auth": auth, "request_template": req, "response_mapping": resp,
			"dlr_mapping": dlr, "tps_limit": tps, "concurrency": conc, "dlr_path": "/dlr/" + id + "?token=" + token, "has_secret": hasSecret}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		writeDBErr(w, err)
		return
	}
	if a.Redis != nil {
		if st, err := vendorhealth.States(r.Context(), a.Redis); err == nil {
			if s, ok := st[id]; ok {
				out["routing"] = s
			}
		}
		if rep, err := vendorhealth.Reports(r.Context(), a.Redis); err == nil {
			out["connectors"] = rep[id]
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) setVendorStatus(w http.ResponseWriter, r *http.Request, act actor) {
	if !platformOnly(w, act) {
		return
	}
	var req struct {
		Status string `json:"status"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Status != "active" && req.Status != "paused" && req.Status != "disabled" {
		writeErr(w, http.StatusBadRequest, "status must be active, paused or disabled")
		return
	}
	tag, err := a.PG.Exec(r.Context(), `UPDATE vendors SET status = $2 WHERE id = $1`, r.PathValue("vendor"), req.Status)
	if err != nil {
		writeDBErr(w, err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeErr(w, http.StatusNotFound, "no such vendor")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": req.Status, "note": "routers apply it within 15 seconds"})
}

var tlvKeys = map[string]bool{"pe_id": true, "template_id": true, "tm_chain": true}

func (a *API) addBind(w http.ResponseWriter, r *http.Request, act actor) {
	if !platformOnly(w, act) {
		return
	}
	vendor := r.PathValue("vendor")
	var proto string
	if err := a.PG.QueryRow(r.Context(), `SELECT protocol FROM vendors WHERE id = $1`, vendor).Scan(&proto); err != nil {
		writeDBErr(w, err)
		return
	}
	if proto != "smpp" {
		writeErr(w, http.StatusBadRequest, "binds are for SMPP vendors; HTTP vendors have a profile")
		return
	}
	var req struct {
		Host        string            `json:"host"`
		Port        int               `json:"port"`
		TLS         bool              `json:"tls"`
		SystemID    string            `json:"system_id"`
		Password    string            `json:"password"`
		SystemType  string            `json:"system_type"`
		SMPPVersion string            `json:"smpp_version"`
		BindType    string            `json:"bind_type"`
		Window      int               `json:"window_size"`
		TPS         int               `json:"tps_limit"`
		TLVProfile  map[string]string `json:"tlv_profile"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.SMPPVersion == "" {
		req.SMPPVersion = "3.4"
	}
	if req.BindType == "" {
		req.BindType = "transceiver"
	}
	if req.Window == 0 {
		req.Window = 10
	}
	if req.TPS == 0 {
		req.TPS = 100
	}
	switch {
	case strings.TrimSpace(req.Host) == "" || req.Port < 1 || req.Port > 65535:
		writeErr(w, http.StatusBadRequest, "host and port (1-65535) are required")
		return
	case req.SystemID == "" || len(req.SystemID) > 16 || req.Password == "" || len(req.Password) > 9:
		writeErr(w, http.StatusBadRequest, "system_id (up to 16) and password (up to 9 characters) are required")
		return
	case req.SMPPVersion != "3.3" && req.SMPPVersion != "3.4" && req.SMPPVersion != "5.0":
		writeErr(w, http.StatusBadRequest, "smpp_version must be 3.3, 3.4 or 5.0")
		return
	case req.BindType != "transmitter" && req.BindType != "receiver" && req.BindType != "transceiver":
		writeErr(w, http.StatusBadRequest, "bind_type must be transmitter, receiver or transceiver")
		return
	case req.Window < 1 || req.Window > 1000 || req.TPS < 1:
		writeErr(w, http.StatusBadRequest, "window_size (1-1000) and tps_limit must be positive")
		return
	}
	for k, v := range req.TLVProfile {
		if !tlvKeys[k] {
			writeErr(w, http.StatusBadRequest, "tlv_profile keys: pe_id, template_id, tm_chain")
			return
		}
		if n, err := strconv.ParseUint(strings.TrimPrefix(v, "0x"), 16, 16); err != nil || n == 0 {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("tlv_profile %s: %q is not a TLV tag like 0x1400", k, v))
			return
		}
	}
	if a.Secrets == nil {
		writeErr(w, http.StatusServiceUnavailable, "SECRETS_KEY is not configured")
		return
	}
	enc, err := a.Secrets.Seal([]byte(req.Password))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not encrypt the password")
		return
	}
	tlv, _ := json.Marshal(req.TLVProfile)
	if req.TLVProfile == nil {
		tlv = []byte("{}")
	}
	var id string
	err = a.PG.QueryRow(r.Context(), `INSERT INTO vendor_binds (vendor_id, host, port, tls, system_id, password_enc, system_type,
		smpp_version, bind_type, window_size, tps_limit, tlv_profile) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12) RETURNING id`,
		vendor, req.Host, req.Port, req.TLS, req.SystemID, enc, req.SystemType, req.SMPPVersion, req.BindType, req.Window, req.TPS, tlv).Scan(&id)
	if err != nil {
		writeDBErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": id, "note": "the vendor's connector picks up bind changes when it restarts"})
}

func (a *API) disableBind(w http.ResponseWriter, r *http.Request, act actor) {
	if !platformOnly(w, act) {
		return
	}
	tag, err := a.PG.Exec(r.Context(), `UPDATE vendor_binds SET enabled = false WHERE id = $1 AND vendor_id = $2 AND enabled`,
		r.PathValue("bind"), r.PathValue("vendor"))
	if err != nil {
		writeDBErr(w, err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeErr(w, http.StatusNotFound, "no enabled bind with this id")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "disabled", "note": "takes effect when the connector restarts"})
}

// putHTTPProfile sets an HTTP vendor's request and receipt mapping,
// validated exactly as the connector will load it.
func (a *API) putHTTPProfile(w http.ResponseWriter, r *http.Request, act actor) {
	if !platformOnly(w, act) {
		return
	}
	vendor := r.PathValue("vendor")
	var proto string
	if err := a.PG.QueryRow(r.Context(), `SELECT protocol FROM vendors WHERE id = $1`, vendor).Scan(&proto); err != nil {
		writeDBErr(w, err)
		return
	}
	if proto != "http" {
		writeErr(w, http.StatusBadRequest, "profiles are for HTTP vendors")
		return
	}
	var req struct {
		BaseURL         string          `json:"base_url"`
		Auth            json.RawMessage `json:"auth"`
		RequestTemplate json.RawMessage `json:"request_template"`
		ResponseMapping json.RawMessage `json:"response_mapping"`
		DLRMapping      json.RawMessage `json:"dlr_mapping"`
		TPS             int             `json:"tps_limit"`
		Concurrency     int             `json:"concurrency"`
		Secret          *string         `json:"secret"` // omitted keeps the stored one
	}
	if !decode(w, r, &req) {
		return
	}
	if len(req.Auth) == 0 {
		req.Auth = json.RawMessage(`{}`)
	}
	if req.TPS == 0 {
		req.TPS = 100
	}
	if req.Concurrency == 0 {
		req.Concurrency = 50
	}
	if u, err := url.Parse(req.BaseURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		writeErr(w, http.StatusBadRequest, "base_url must be an http(s) URL")
		return
	}
	if len(req.RequestTemplate) == 0 || len(req.ResponseMapping) == 0 || len(req.DLRMapping) == 0 {
		writeErr(w, http.StatusBadRequest, "request_template, response_mapping and dlr_mapping are required")
		return
	}
	if req.TPS < 1 || req.Concurrency < 1 {
		writeErr(w, http.StatusBadRequest, "tps_limit and concurrency must be positive")
		return
	}
	if _, err := connector.ParseHTTPProfile(req.BaseURL, req.Auth, req.RequestTemplate, req.ResponseMapping, req.DLRMapping); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var enc []byte
	if req.Secret != nil && *req.Secret != "" {
		if a.Secrets == nil {
			writeErr(w, http.StatusServiceUnavailable, "SECRETS_KEY is not configured")
			return
		}
		var err error
		if enc, err = a.Secrets.Seal([]byte(*req.Secret)); err != nil {
			writeErr(w, http.StatusInternalServerError, "could not encrypt the secret")
			return
		}
	}
	_, err := a.PG.Exec(r.Context(), `INSERT INTO http_vendor_profiles (vendor_id, base_url, auth, request_template, response_mapping,
		dlr_mapping, tps_limit, concurrency, secret_enc) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (vendor_id) DO UPDATE SET base_url = EXCLUDED.base_url, auth = EXCLUDED.auth, request_template = EXCLUDED.request_template,
		  response_mapping = EXCLUDED.response_mapping, dlr_mapping = EXCLUDED.dlr_mapping, tps_limit = EXCLUDED.tps_limit,
		  concurrency = EXCLUDED.concurrency, secret_enc = coalesce(EXCLUDED.secret_enc, http_vendor_profiles.secret_enc)`,
		vendor, req.BaseURL, req.Auth, req.RequestTemplate, req.ResponseMapping, req.DLRMapping, req.TPS, req.Concurrency, enc)
	if err != nil {
		writeDBErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "saved", "note": "the vendor's connector picks it up when it restarts"})
}

func (a *API) listVendorCosts(w http.ResponseWriter, r *http.Request, act actor) {
	if !platformOnly(w, act) {
		return
	}
	rows, err := a.PG.Query(r.Context(), `SELECT mcc, mnc, category, cost, valid_from FROM vendor_costs WHERE vendor_id = $1
		ORDER BY mcc, mnc, category, valid_from DESC LIMIT 5000`, r.PathValue("vendor"))
	if err != nil {
		writeDBErr(w, err)
		return
	}
	defer rows.Close()
	now := time.Now()
	seen := map[string]bool{}
	out := []map[string]any{}
	for rows.Next() {
		var mcc, mnc, cat string
		var cost decimal.Decimal
		var from time.Time
		if err := rows.Scan(&mcc, &mnc, &cat, &cost, &from); err != nil {
			writeDBErr(w, err)
			return
		}
		k := mcc + "|" + mnc + "|" + cat
		state := "scheduled"
		if !from.After(now) {
			state = "superseded"
			if !seen[k] {
				state, seen[k] = "current", true
			}
		}
		out = append(out, map[string]any{"mcc": mcc, "mnc": mnc, "category": cat, "cost": cost.StringFixed(4), "valid_from": from, "state": state})
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) listVendorInvoices(w http.ResponseWriter, r *http.Request, act actor) {
	if !platformOnly(w, act) {
		return
	}
	rows, err := a.PG.Query(r.Context(), `SELECT i.id, i.vendor_id, i.reference, i.period_from, i.period_to, i.total_segments, i.total_amount,
		i.imported_by, i.imported_at, coalesce((SELECT d.status FROM vendor_disputes d WHERE d.invoice_id = i.id ORDER BY d.created_at DESC LIMIT 1), '')
		FROM vendor_invoices i WHERE ($1 = '' OR i.vendor_id = $1) ORDER BY i.period_from DESC, i.imported_at DESC LIMIT 500`, r.URL.Query().Get("vendor"))
	if err != nil {
		writeDBErr(w, err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, vendor, ref, by, dispute string
		var from, to, at time.Time
		var segs int64
		var amt decimal.Decimal
		if err := rows.Scan(&id, &vendor, &ref, &from, &to, &segs, &amt, &by, &at, &dispute); err != nil {
			writeDBErr(w, err)
			return
		}
		out = append(out, map[string]any{"id": id, "vendor_id": vendor, "reference": ref, "period_from": from.Format(time.DateOnly),
			"period_to": to.Format(time.DateOnly), "total_segments": segs, "total_amount": amt.StringFixed(4), "imported_by": by,
			"imported_at": at, "dispute_status": dispute})
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) getFirewallLimits(w http.ResponseWriter, r *http.Request, act actor) {
	id, ok := a.managedOrSelf(w, r, act)
	if !ok {
		return
	}
	l := firewall.DefaultLimits
	custom := true
	err := a.PG.QueryRow(r.Context(), `SELECT dest_per_hour, series_per_10min, series_block_minutes FROM firewall_limits WHERE account_id = $1`, id).
		Scan(&l.DestPerHour, &l.SeriesPer10Min, &l.SeriesBlockMins)
	if errors.Is(err, pgx.ErrNoRows) {
		custom = false
	} else if err != nil {
		writeDBErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"dest_per_hour": l.DestPerHour, "series_per_10min": l.SeriesPer10Min,
		"series_block_minutes": l.SeriesBlockMins, "custom": custom})
}

func (a *API) listHeaders(w http.ResponseWriter, r *http.Request, act actor) {
	acct, ok := a.managedOrSelf(w, r, act)
	if !ok {
		return
	}
	ent, ok := a.entityOf(w, r, acct)
	if !ok {
		return
	}
	rows, err := a.PG.Query(r.Context(), `SELECT h.header, h.category, h.status,
		(SELECT count(*) FROM dlt_template_headers th JOIN dlt_templates t ON t.id = th.template_id WHERE th.header_id = h.id AND t.status = 'active')
		FROM dlt_headers h WHERE h.entity_id = $1 ORDER BY h.header`, ent)
	if err != nil {
		writeDBErr(w, err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var h, cat, st string
		var n int
		if err := rows.Scan(&h, &cat, &st, &n); err != nil {
			writeDBErr(w, err)
			return
		}
		out = append(out, map[string]any{"header": h, "category": cat, "status": st, "active_templates": n})
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) listSeries(w http.ResponseWriter, r *http.Request, act actor) {
	if !platformOnly(w, act) {
		return
	}
	var total int
	_ = a.PG.QueryRow(r.Context(), `SELECT count(*) FROM number_series`).Scan(&total)
	rows, err := a.PG.Query(r.Context(), `SELECT prefix, mcc, mnc, operator, circle FROM number_series
		WHERE $1 = '' OR prefix LIKE $1 || '%' OR operator ILIKE '%' || $1 || '%' OR circle ILIKE '%' || $1 || '%'
		ORDER BY prefix LIMIT 500`, r.URL.Query().Get("q"))
	if err != nil {
		writeDBErr(w, err)
		return
	}
	defer rows.Close()
	out := []map[string]string{}
	for rows.Next() {
		var p, mcc, mnc, op, circle string
		if err := rows.Scan(&p, &mcc, &mnc, &op, &circle); err != nil {
			writeDBErr(w, err)
			return
		}
		out = append(out, map[string]string{"prefix": p, "mcc": mcc, "mnc": mnc, "operator": op, "circle": circle})
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": total, "series": out})
}

func (a *API) listMNPImports(w http.ResponseWriter, r *http.Request, act actor) {
	if !platformOnly(w, act) {
		return
	}
	rows, err := a.PG.Query(r.Context(), `SELECT rows, bad_lines, imported_by, imported_at FROM mnp_imports ORDER BY imported_at DESC LIMIT 50`)
	if err != nil {
		writeDBErr(w, err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var n int64
		var bad int
		var by string
		var at time.Time
		if err := rows.Scan(&n, &bad, &by, &at); err != nil {
			writeDBErr(w, err)
			return
		}
		out = append(out, map[string]any{"rows": n, "bad_lines": bad, "imported_by": by, "imported_at": at})
	}
	writeJSON(w, http.StatusOK, out)
}
