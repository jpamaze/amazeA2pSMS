package adminapi

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jpamaze/amazeA2pSMS/internal/dlt"
	"github.com/jpamaze/amazeA2pSMS/internal/domain"
)

// dltRoutes registers DLT registrations, checks and reports (phase 3).
// An account manages its own DLT data, and so do its managers (the
// platform, or its reseller).
func (a *API) dltRoutes(mux *http.ServeMux) {
	mux.Handle("GET /v1/accounts/{id}/dlt", a.authed(PermRead, a.dltSummary))
	mux.Handle("POST /v1/accounts/{id}/dlt/entities", a.authed(PermAdmin, a.putEntity))
	mux.Handle("PUT /v1/accounts/{id}/dlt/entities/{pe}/chain", a.authed(PermAdmin, a.putChain))
	mux.Handle("POST /v1/accounts/{id}/dlt/entities/{pe}/headers/import", a.authed(PermAdmin, a.importHeaders))
	mux.Handle("POST /v1/accounts/{id}/dlt/entities/{pe}/templates/import", a.authed(PermAdmin, a.importTemplates))
	mux.Handle("GET /v1/accounts/{id}/dlt/entities/{pe}/templates", a.authed(PermRead, a.listTemplates))
	mux.Handle("POST /v1/accounts/{id}/dlt/check", a.authed(PermRead, a.dltCheck))
	mux.Handle("GET /v1/accounts/{id}/dlt/report", a.authed(PermRead, a.dltReport))
	mux.Handle("PUT /v1/accounts/{id}/dlt-mode", a.authed(PermAdmin, a.setDLTMode))
	mux.Handle("POST /v1/vendor-error-codes/import", a.authed(PermAdmin, a.importErrorCodes))
	mux.Handle("GET /v1/vendor-error-codes", a.authed(PermRead, a.listErrorCodes))
}

var peRe = regexp.MustCompile(`^[0-9]{10,30}$`)

// entityOf returns the entity row ID for the path PE ID under the account.
func (a *API) entityOf(w http.ResponseWriter, r *http.Request, acct string) (string, bool) {
	var id string
	err := a.PG.QueryRow(r.Context(), `SELECT id FROM dlt_entities WHERE account_id = $1 AND pe_id = $2`, acct, r.PathValue("pe")).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "PE ID not registered for this account")
		return "", false
	}
	if err != nil {
		writeDBErr(w, err)
		return "", false
	}
	return id, true
}

func (a *API) putEntity(w http.ResponseWriter, r *http.Request, act actor) {
	acct, ok := a.managedOrSelf(w, r, act)
	if !ok {
		return
	}
	var req struct {
		PEID    string `json:"pe_id"`
		Name    string `json:"name"`
		Status  string `json:"status"`
		TMChain string `json:"tm_chain"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Status == "" {
		req.Status = "active"
	}
	if !peRe.MatchString(req.PEID) || strings.TrimSpace(req.Name) == "" || (req.Status != "active" && req.Status != "suspended") {
		writeErr(w, http.StatusBadRequest, "pe_id (10-30 digits), name and status active|suspended are required")
		return
	}
	var id, owner string
	err := pgx.BeginFunc(r.Context(), a.PG, func(tx pgx.Tx) error {
		if err := tx.QueryRow(r.Context(), `INSERT INTO dlt_entities (account_id, pe_id, name, status) VALUES ($1, $2, $3, $4)
			ON CONFLICT (pe_id) DO UPDATE SET name = EXCLUDED.name, status = EXCLUDED.status
			RETURNING id, account_id`, acct, req.PEID, req.Name, req.Status).Scan(&id, &owner); err != nil {
			return err
		}
		if owner != acct {
			return errConflict
		}
		if req.TMChain != "" {
			return setChain(r, tx, id, req.TMChain)
		}
		return nil
	})
	if errors.Is(err, errConflict) {
		writeErr(w, http.StatusConflict, "this PE ID is registered to another account")
		return
	}
	if err != nil {
		writeDBErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"pe_id": req.PEID, "status": req.Status, "note": "edges apply changes within one minute"})
}

func setChain(r *http.Request, tx pgx.Tx, entityID, chain string) error {
	if _, err := tx.Exec(r.Context(), `UPDATE dlt_tm_chains SET valid_to = now()
		WHERE entity_id = $1 AND (valid_to IS NULL OR valid_to > now())`, entityID); err != nil {
		return err
	}
	_, err := tx.Exec(r.Context(), `INSERT INTO dlt_tm_chains (entity_id, chain_value) VALUES ($1, $2)`, entityID, chain)
	return err
}

func (a *API) putChain(w http.ResponseWriter, r *http.Request, act actor) {
	acct, ok := a.managedOrSelf(w, r, act)
	if !ok {
		return
	}
	ent, ok := a.entityOf(w, r, acct)
	if !ok {
		return
	}
	var req struct {
		TMChain string `json:"tm_chain"`
	}
	if !decode(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.TMChain) == "" || len(req.TMChain) > 1000 {
		writeErr(w, http.StatusBadRequest, "tm_chain is required (the PE-TM chain value registered on the DLT portal)")
		return
	}
	if err := pgx.BeginFunc(r.Context(), a.PG, func(tx pgx.Tx) error { return setChain(r, tx, ent, strings.TrimSpace(req.TMChain)) }); err != nil {
		writeDBErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}

func (a *API) importHeaders(w http.ResponseWriter, r *http.Request, act actor) {
	a.importDLT(w, r, act, func(tx pgx.Tx, ent string, body io.Reader) (int, error) {
		rows, err := dlt.ParseHeaders(body)
		if err != nil {
			return 0, err
		}
		return len(rows), dlt.StoreHeaders(r.Context(), tx, ent, rows)
	})
}

func (a *API) importTemplates(w http.ResponseWriter, r *http.Request, act actor) {
	a.importDLT(w, r, act, func(tx pgx.Tx, ent string, body io.Reader) (int, error) {
		rows, err := dlt.ParseTemplates(body)
		if err != nil {
			return 0, err
		}
		return len(rows), dlt.StoreTemplates(r.Context(), tx, ent, rows)
	})
}

// importDLT runs one CSV import in a transaction: all lines or none.
func (a *API) importDLT(w http.ResponseWriter, r *http.Request, act actor, run func(pgx.Tx, string, io.Reader) (int, error)) {
	acct, ok := a.managedOrSelf(w, r, act)
	if !ok {
		return
	}
	ent, ok := a.entityOf(w, r, acct)
	if !ok {
		return
	}
	var n int
	err := pgx.BeginFunc(r.Context(), a.PG, func(tx pgx.Tx) error {
		var err error
		n, err = run(tx, ent, io.LimitReader(r.Body, 32<<20))
		return err
	})
	var lines dlt.LineErrors
	if errors.As(err, &lines) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "nothing imported", "lines": []string(lines)})
		return
	}
	if err != nil && strings.HasPrefix(err.Error(), "CSV") {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err != nil {
		writeDBErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"imported": n, "note": "edges apply changes within one minute"})
}

func (a *API) listTemplates(w http.ResponseWriter, r *http.Request, act actor) {
	acct, ok := a.managedOrSelf(w, r, act)
	if !ok {
		return
	}
	ent, ok := a.entityOf(w, r, acct)
	if !ok {
		return
	}
	rows, err := a.PG.Query(r.Context(), `SELECT t.template_id, t.name, t.category, t.status, t.body,
		coalesce(array_agg(DISTINCT h.header) FILTER (WHERE h.header IS NOT NULL), '{}'),
		coalesce(array_agg(DISTINCT w.kind || ':' || w.value) FILTER (WHERE w.value IS NOT NULL), '{}')
		FROM dlt_templates t
		LEFT JOIN dlt_template_headers th ON th.template_id = t.id LEFT JOIN dlt_headers h ON h.id = th.header_id
		LEFT JOIN dlt_template_whitelist w ON w.template_id = t.id
		WHERE t.entity_id = $1 AND ($2 = '' OR upper(h.header) = upper($2))
		GROUP BY t.id ORDER BY t.template_id LIMIT 5000`, ent, r.URL.Query().Get("header"))
	if err != nil {
		writeDBErr(w, err)
		return
	}
	defer rows.Close()
	type tpl struct {
		TemplateID string   `json:"template_id"`
		Name       string   `json:"name"`
		Category   string   `json:"category"`
		Status     string   `json:"status"`
		Body       string   `json:"body"`
		Headers    []string `json:"headers"`
		Whitelist  []string `json:"whitelist"`
	}
	out := []tpl{}
	for rows.Next() {
		var t tpl
		if err := rows.Scan(&t.TemplateID, &t.Name, &t.Category, &t.Status, &t.Body, &t.Headers, &t.Whitelist); err != nil {
			writeDBErr(w, err)
			return
		}
		out = append(out, t)
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) dltSummary(w http.ResponseWriter, r *http.Request, act actor) {
	acct, ok := a.managedOrSelf(w, r, act)
	if !ok {
		return
	}
	var mode string
	if err := a.PG.QueryRow(r.Context(), `SELECT dlt_mode FROM accounts WHERE id = $1`, acct).Scan(&mode); err != nil {
		writeDBErr(w, err)
		return
	}
	rows, err := a.PG.Query(r.Context(), `SELECT e.pe_id, e.name, e.status,
		(SELECT count(*) FROM dlt_headers h WHERE h.entity_id = e.id),
		(SELECT count(*) FROM dlt_templates t WHERE t.entity_id = e.id AND t.status = 'active'),
		coalesce((SELECT c.chain_value FROM dlt_tm_chains c WHERE c.entity_id = e.id AND c.valid_from <= now()
		  AND (c.valid_to IS NULL OR c.valid_to > now()) ORDER BY c.valid_from DESC LIMIT 1), '')
		FROM dlt_entities e WHERE e.account_id = $1 ORDER BY e.pe_id`, acct)
	if err != nil {
		writeDBErr(w, err)
		return
	}
	defer rows.Close()
	type ent struct {
		PEID      string `json:"pe_id"`
		Name      string `json:"name"`
		Status    string `json:"status"`
		Headers   int    `json:"headers"`
		Templates int    `json:"active_templates"`
		TMChain   string `json:"tm_chain"`
	}
	out := []ent{}
	for rows.Next() {
		var e ent
		if err := rows.Scan(&e.PEID, &e.Name, &e.Status, &e.Headers, &e.Templates, &e.TMChain); err != nil {
			writeDBErr(w, err)
			return
		}
		out = append(out, e)
	}
	writeJSON(w, http.StatusOK, map[string]any{"account_id": acct, "dlt_mode": mode, "entities": out})
}

// dltCheck runs the submit-time check against current registrations
// without sending anything.
func (a *API) dltCheck(w http.ResponseWriter, r *http.Request, act actor) {
	acct, ok := a.managedOrSelf(w, r, act)
	if !ok {
		return
	}
	var req struct {
		Sender     string `json:"sender"`
		Text       string `json:"text"`
		PEID       string `json:"pe_id"`
		TemplateID string `json:"template_id"`
		TMChain    string `json:"tm_chain"`
		At         string `json:"at"` // RFC 3339; checks promotional hours at that time
	}
	if !decode(w, r, &req) {
		return
	}
	c := &dlt.Checker{PG: a.PG, TTL: -1, RequireChain: true}
	if req.At != "" {
		at, err := time.Parse(time.RFC3339, req.At)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "at must be RFC 3339")
			return
		}
		c.Now = func() time.Time { return at }
	}
	f, err := c.Check(r.Context(), acct, req.Sender, req.Text, false,
		domain.DLTFields{PEID: req.PEID, TemplateID: req.TemplateID, TMChain: req.TMChain})
	if e, ok := dlt.AsError(err); ok {
		info := dlt.Catalog[e.Reason]
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "reason": e.Reason, "detail": e.Detail,
			"receipt_code": info.Code, "smpp_status": fmt.Sprintf("0x%08X", info.SMPPStatus)})
		return
	}
	if err != nil {
		writeDBErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "pe_id": f.PEID, "template_id": f.TemplateID,
		"category": f.Category, "tm_chain": f.TMChain})
}

// dltReport counts messages by PE ID, header, template or category, with
// operator DLT failures (receipt codes mapped to a dlt_ reason).
func (a *API) dltReport(w http.ResponseWriter, r *http.Request, act actor) {
	acct, ok := a.managedOrSelf(w, r, act)
	if !ok {
		return
	}
	if a.CH == nil {
		writeErr(w, http.StatusServiceUnavailable, "DLT reports need CLICKHOUSE_URL")
		return
	}
	from, to, err := period(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	col := map[string]string{"pe_id": "pe_id", "header": "sender", "template": "template_id", "category": "dlt_category"}[r.URL.Query().Get("by")]
	if col == "" {
		col = "template_id"
	}
	// Column names come from the fixed map above; the account ID was checked.
	rows, err := a.CH.Query(r.Context(), fmt.Sprintf(`SELECT %[1]s AS k, count(), sum(segments),
		countIf(status = 'delivered'), countIf(status IN ('failed', 'expired', 'rejected')),
		countIf(position(error_code, '|dlt_') > 0),
		arrayElement(topKIf(1)(extract(error_code, '\\|(dlt_[a-z_]+)'), position(error_code, '|dlt_') > 0), 1)
		FROM message_records FINAL
		WHERE account_id = toUUID('%[2]s') AND accepted_at >= %[3]s AND accepted_at < %[4]s
		GROUP BY k ORDER BY count() DESC LIMIT 1000`, col, acct, chTime(from), chTime(to)))
	if err != nil {
		a.Log.Error("dlt report", "err", err)
		writeErr(w, http.StatusServiceUnavailable, "usage store unavailable")
		return
	}
	out := []map[string]string{}
	for _, row := range rows {
		if len(row) == 7 {
			out = append(out, map[string]string{"key": row[0], "messages": row[1], "segments": row[2], "delivered": row[3],
				"not_delivered": row[4], "dlt_failures": row[5], "top_dlt_reason": row[6]})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"account_id": acct, "by": col, "from": from.Format(time.RFC3339),
		"to": to.Format(time.RFC3339), "tz": from.Location().String(), "rows": out,
		"note": "messages refused at submit are not in this report; see sms_refused_total{reason=~\"dlt_.*\"}"})
}

func (a *API) setDLTMode(w http.ResponseWriter, r *http.Request, act actor) {
	if !platformOnly(w, act) {
		return
	}
	var req struct {
		Mode   string `json:"mode"`
		Reason string `json:"reason"`
	}
	if !decode(w, r, &req) {
		return
	}
	if (req.Mode != "enforce" && req.Mode != "off") || strings.TrimSpace(req.Reason) == "" {
		writeErr(w, http.StatusBadRequest, "mode enforce|off and a reason are required")
		return
	}
	tag, err := a.PG.Exec(r.Context(), `UPDATE accounts SET dlt_mode = $2 WHERE id = $1`, r.PathValue("id"), req.Mode)
	if err != nil {
		writeDBErr(w, err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeErr(w, http.StatusNotFound, "no such account")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"dlt_mode": req.Mode})
}

// importErrorCodes reads code,reason,customer_code[,description]; ?vendor=
// scopes the rows to one vendor (default: every vendor).
func (a *API) importErrorCodes(w http.ResponseWriter, r *http.Request, act actor) {
	if !platformOnly(w, act) {
		return
	}
	var vendor *string
	if v := r.URL.Query().Get("vendor"); v != "" {
		vendor = &v
	}
	cr := csv.NewReader(io.LimitReader(r.Body, 4<<20))
	cr.FieldsPerRecord = -1
	cr.TrimLeadingSpace = true
	recs, err := cr.ReadAll()
	if err != nil {
		writeErr(w, http.StatusBadRequest, "CSV: "+err.Error())
		return
	}
	if len(recs) > 0 && strings.EqualFold(strings.TrimSpace(recs[0][0]), "code") {
		recs = recs[1:]
	}
	reasonRe := regexp.MustCompile(`^[a-z0-9_]{2,40}$`)
	codeRe := regexp.MustCompile(`^[0-9]{3}$`)
	var bad []string
	for i, rec := range recs {
		if len(rec) < 3 || strings.TrimSpace(rec[0]) == "" || !reasonRe.MatchString(rec[1]) || !codeRe.MatchString(rec[2]) {
			bad = append(bad, fmt.Sprintf("line %d: want code,reason (a-z0-9_),customer_code (3 digits)[,description]", i+2))
		}
	}
	if len(bad) > 0 || len(recs) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "nothing imported", "lines": bad})
		return
	}
	err = pgx.BeginFunc(r.Context(), a.PG, func(tx pgx.Tx) error {
		for _, rec := range recs {
			desc := ""
			if len(rec) > 3 {
				desc = rec[3]
			}
			if _, err := tx.Exec(r.Context(), `INSERT INTO vendor_error_codes (vendor_id, code, reason, customer_code, description)
				VALUES ($1, $2, $3, $4, $5)
				ON CONFLICT (coalesce(vendor_id, ''), code) DO UPDATE SET reason = EXCLUDED.reason,
				  customer_code = EXCLUDED.customer_code, description = EXCLUDED.description`,
				vendor, strings.TrimSpace(rec[0]), rec[1], rec[2], desc); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		writeDBErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"imported": len(recs), "note": "the DLR processor reloads within one minute"})
}

func (a *API) listErrorCodes(w http.ResponseWriter, r *http.Request, act actor) {
	if !platformOnly(w, act) {
		return
	}
	rows, err := a.PG.Query(r.Context(), `SELECT coalesce(vendor_id, '*'), code, reason, customer_code, description
		FROM vendor_error_codes ORDER BY vendor_id NULLS FIRST, code`)
	if err != nil {
		writeDBErr(w, err)
		return
	}
	defer rows.Close()
	out := []map[string]string{}
	for rows.Next() {
		var v, c, re, cc, d string
		if err := rows.Scan(&v, &c, &re, &cc, &d); err != nil {
			writeDBErr(w, err)
			return
		}
		out = append(out, map[string]string{"vendor": v, "code": c, "reason": re, "customer_code": cc, "description": d})
	}
	writeJSON(w, http.StatusOK, out)
}
