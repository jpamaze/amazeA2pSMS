package adminapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/jpamaze/amazeA2pSMS/internal/msgstore"
	"github.com/jpamaze/amazeA2pSMS/internal/numbering"
)

// portalDataRoutes registers the reads and small changes the portals need
// on top of the management API (phase 4).
func (a *API) portalDataRoutes(mux *http.ServeMux) {
	mux.Handle("GET /v1/accounts", a.authed(PermRead, a.listAccounts))
	mux.Handle("GET /v1/accounts/{id}", a.authed(PermRead, a.getAccount))
	mux.Handle("PUT /v1/accounts/{id}/status", a.authed(PermAdmin, a.setAccountStatus))
	mux.Handle("GET /v1/accounts/{id}/api-keys", a.authed(PermRead, a.listAPIKeys))
	mux.Handle("DELETE /v1/accounts/{id}/api-keys/{key}", a.authed(PermAdmin, a.revokeAPIKey))
	mux.Handle("GET /v1/accounts/{id}/smpp-credentials", a.authed(PermRead, a.listSMPPCredentials))
	mux.Handle("DELETE /v1/accounts/{id}/smpp-credentials/{cred}", a.authed(PermAdmin, a.disableSMPPCredential))
	mux.Handle("GET /v1/accounts/{id}/webhook", a.authed(PermRead, a.getWebhook))
	mux.Handle("GET /v1/accounts/{id}/users", a.authed(PermRead, a.listUsers))
	mux.Handle("DELETE /v1/accounts/{id}/users/{user}", a.authed(PermAdmin, a.disableUser))
	mux.Handle("GET /v1/accounts/{id}/messages", a.authed(PermRead, a.searchMessages))
	mux.Handle("GET /v1/accounts/{id}/messages/{msg}", a.authed(PermRead, a.getMessage))
	mux.Handle("GET /v1/accounts/{id}/dashboard", a.authed(PermRead, a.dashboard))
	mux.Handle("GET /v1/accounts/{id}/traffic", a.authed(PermRead, a.traffic))
	mux.Handle("GET /v1/accounts/{id}/alerts", a.authed(PermRead, a.getAlerts))
	mux.Handle("PUT /v1/accounts/{id}/alerts", a.authed(PermFinance, a.putAlerts))
	mux.Handle("GET /v1/monitor/live", a.authed(PermRead, a.monitorLive))
}

type accountRow struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Role       string `json:"role"`
	Status     string `json:"status"`
	ParentID   string `json:"parent_id,omitempty"`
	ParentName string `json:"parent_name,omitempty"`
	RateCard   string `json:"rate_card,omitempty"`
	DLTMode    string `json:"dlt_mode"`
	CreatedAt  string `json:"created_at"`
	Balance    string `json:"balance"`
	Credit     string `json:"credit_limit"`
	Available  string `json:"available,omitempty"` // live, from Redis
}

const accountSelect = `SELECT a.id, a.name, a.role, a.status, coalesce(a.parent_id::text, ''), coalesce(p.name, ''),
	coalesce(rc.name, ''), a.dlt_mode, a.created_at, w.balance, w.credit_limit
	FROM accounts a LEFT JOIN accounts p ON p.id = a.parent_id LEFT JOIN rate_cards rc ON rc.id = a.rate_card_id
	JOIN wallets w ON w.account_id = a.id`

func scanAccount(row pgx.Row) (accountRow, error) {
	var x accountRow
	var created time.Time
	var bal, limit decimal.Decimal
	err := row.Scan(&x.ID, &x.Name, &x.Role, &x.Status, &x.ParentID, &x.ParentName, &x.RateCard, &x.DLTMode, &created, &bal, &limit)
	x.CreatedAt, x.Balance, x.Credit = created.UTC().Format(time.RFC3339), bal.StringFixed(2), limit.StringFixed(2)
	return x, err
}

// listAccounts: the platform sees every account (filter by parent, role,
// name); a reseller its sub-accounts; a customer itself.
func (a *API) listAccounts(w http.ResponseWriter, r *http.Request, act actor) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	offset, _ := strconv.Atoi(q.Get("offset"))
	where, args := []string{"true"}, []any{}
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, fmt.Sprintf(cond, len(args)))
	}
	switch act.Role {
	case "platform":
		if p := q.Get("parent"); p != "" {
			add("a.parent_id::text = $%d", p)
		}
	case "reseller":
		add("a.parent_id = $%d", act.AccountID)
	default:
		add("a.id = $%d", act.AccountID)
	}
	if v := q.Get("role"); v != "" {
		add("a.role = $%d", v)
	}
	if v := strings.TrimSpace(q.Get("q")); v != "" {
		add("a.name ILIKE '%%' || $%d || '%%'", v)
	}
	rows, err := a.PG.Query(r.Context(), accountSelect+` WHERE `+strings.Join(where, " AND ")+
		fmt.Sprintf(` ORDER BY a.role, a.name LIMIT %d OFFSET %d`, limit, max(offset, 0)), args...)
	if err != nil {
		writeDBErr(w, err)
		return
	}
	defer rows.Close()
	out := []accountRow{}
	for rows.Next() {
		x, err := scanAccount(rows)
		if err != nil {
			writeDBErr(w, err)
			return
		}
		out = append(out, x)
	}
	if err := rows.Err(); err != nil {
		writeDBErr(w, err)
		return
	}
	a.fillAvailable(r.Context(), out)
	writeJSON(w, http.StatusOK, out)
}

// fillAvailable adds each wallet's live available balance (Redis), in parallel.
func (a *API) fillAvailable(ctx context.Context, rows []accountRow) {
	if a.Wallets == nil {
		return
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 16)
	for i := range rows {
		wg.Add(1)
		sem <- struct{}{}
		go func(x *accountRow) {
			defer wg.Done()
			defer func() { <-sem }()
			if s, err := a.Wallets.Get(ctx, x.ID); err == nil {
				x.Available = s.Available().Decimal().StringFixed(2)
			}
		}(&rows[i])
	}
	wg.Wait()
}

func (a *API) getAccount(w http.ResponseWriter, r *http.Request, act actor) {
	id, ok := a.managedOrSelf(w, r, act)
	if !ok {
		return
	}
	x, err := scanAccount(a.PG.QueryRow(r.Context(), accountSelect+` WHERE a.id = $1`, id))
	if err != nil {
		writeDBErr(w, err)
		return
	}
	rows := []accountRow{x}
	a.fillAvailable(r.Context(), rows)
	var subs int
	_ = a.PG.QueryRow(r.Context(), `SELECT count(*) FROM accounts WHERE parent_id = $1`, id).Scan(&subs)
	writeJSON(w, http.StatusOK, map[string]any{"account": rows[0], "sub_accounts": subs})
}

func (a *API) setAccountStatus(w http.ResponseWriter, r *http.Request, act actor) {
	id, ok := a.managed(w, r, act)
	if !ok {
		return
	}
	if id == act.AccountID {
		writeErr(w, http.StatusBadRequest, "you cannot suspend your own account")
		return
	}
	var req struct {
		Status string `json:"status"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Status != "active" && req.Status != "suspended" {
		writeErr(w, http.StatusBadRequest, "status must be active or suspended")
		return
	}
	// A suspension is lifted only by whoever imposed it, or the platform.
	// Suspending an account that is already suspended keeps the original
	// suspender (so a reseller cannot take over a platform suspension); the
	// platform always takes it over.
	tag, err := a.PG.Exec(r.Context(), `UPDATE accounts
		SET status = $2, suspended_by = CASE WHEN $2 = 'suspended' THEN
		      CASE WHEN $4 OR status <> 'suspended' OR suspended_by IS NULL THEN $3::uuid ELSE suspended_by END
		    END
		WHERE id = $1 AND status <> 'closed'
		  AND ($2 = 'suspended' OR suspended_by IS NULL OR suspended_by = $3::uuid OR $4)`,
		id, req.Status, act.AccountID, act.Role == "platform")
	if err != nil {
		writeDBErr(w, err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeErr(w, http.StatusForbidden, "this account was suspended by the platform; only the platform can reactivate it")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": req.Status, "note": "sending stops or resumes within one minute"})
}

func (a *API) listAPIKeys(w http.ResponseWriter, r *http.Request, act actor) {
	id, ok := a.managedOrSelf(w, r, act)
	if !ok {
		return
	}
	rows, err := a.PG.Query(r.Context(), `SELECT id, prefix, name, created_at, revoked_at FROM api_keys WHERE account_id = $1 ORDER BY created_at DESC`, id)
	if err != nil {
		writeDBErr(w, err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var kid, prefix, name string
		var created time.Time
		var revoked *time.Time
		if err := rows.Scan(&kid, &prefix, &name, &created, &revoked); err != nil {
			writeDBErr(w, err)
			return
		}
		out = append(out, map[string]any{"id": kid, "prefix": prefix, "name": name, "created_at": created, "revoked_at": revoked})
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) revokeAPIKey(w http.ResponseWriter, r *http.Request, act actor) {
	id, ok := a.managedOrSelf(w, r, act)
	if !ok {
		return
	}
	tag, err := a.PG.Exec(r.Context(), `UPDATE api_keys SET revoked_at = now() WHERE id = $1 AND account_id = $2 AND revoked_at IS NULL`,
		r.PathValue("key"), id)
	if err != nil {
		writeDBErr(w, err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeErr(w, http.StatusNotFound, "no active key with this id")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked", "note": "refused from the next request (keys are cached up to a minute)"})
}

func (a *API) listSMPPCredentials(w http.ResponseWriter, r *http.Request, act actor) {
	id, ok := a.managedOrSelf(w, r, act)
	if !ok {
		return
	}
	rows, err := a.PG.Query(r.Context(), `SELECT id, system_id, coalesce(array_to_string(allowed_ips, ','), ''), max_binds, tps_limit,
		created_at, disabled_at FROM smpp_credentials WHERE account_id = $1 ORDER BY created_at DESC`, id)
	if err != nil {
		writeDBErr(w, err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var cid, sys, ips string
		var binds, tps int
		var created time.Time
		var disabled *time.Time
		if err := rows.Scan(&cid, &sys, &ips, &binds, &tps, &created, &disabled); err != nil {
			writeDBErr(w, err)
			return
		}
		out = append(out, map[string]any{"id": cid, "system_id": sys, "allowed_ips": ips, "max_binds": binds, "tps_limit": tps,
			"created_at": created, "disabled_at": disabled})
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) disableSMPPCredential(w http.ResponseWriter, r *http.Request, act actor) {
	id, ok := a.managedOrSelf(w, r, act)
	if !ok {
		return
	}
	tag, err := a.PG.Exec(r.Context(), `UPDATE smpp_credentials SET disabled_at = now() WHERE id = $1 AND account_id = $2 AND disabled_at IS NULL`,
		r.PathValue("cred"), id)
	if err != nil {
		writeDBErr(w, err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeErr(w, http.StatusNotFound, "no active credential with this id")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "disabled", "note": "new binds are refused; open binds end at their next re-bind"})
}

func (a *API) getWebhook(w http.ResponseWriter, r *http.Request, act actor) {
	id, ok := a.managedOrSelf(w, r, act)
	if !ok {
		return
	}
	var u *string
	if err := a.PG.QueryRow(r.Context(), `SELECT webhook_url FROM accounts WHERE id = $1`, id).Scan(&u); err != nil {
		writeDBErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"url": u})
}

func (a *API) listUsers(w http.ResponseWriter, r *http.Request, act actor) {
	id, ok := a.managedOrSelf(w, r, act)
	if !ok {
		return
	}
	rows, err := a.PG.Query(r.Context(), `SELECT id, email, role, created_at, last_login_at, disabled_at FROM users
		WHERE account_id = $1 ORDER BY disabled_at NULLS FIRST, email`, id)
	if err != nil {
		writeDBErr(w, err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var uid, email, role string
		var created time.Time
		var login, disabled *time.Time
		if err := rows.Scan(&uid, &email, &role, &created, &login, &disabled); err != nil {
			writeDBErr(w, err)
			return
		}
		out = append(out, map[string]any{"id": uid, "email": email, "role": role, "created_at": created,
			"last_login_at": login, "disabled_at": disabled})
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) disableUser(w http.ResponseWriter, r *http.Request, act actor) {
	id, ok := a.managedOrSelf(w, r, act)
	if !ok {
		return
	}
	var email, role string
	err := a.PG.QueryRow(r.Context(), `SELECT email, role FROM users WHERE id = $1 AND account_id = $2 AND disabled_at IS NULL`,
		r.PathValue("user"), id).Scan(&email, &role)
	if err != nil {
		writeDBErr(w, err)
		return
	}
	switch {
	case strings.EqualFold(email, act.User):
		writeErr(w, http.StatusBadRequest, "you cannot remove yourself")
		return
	case role == "owner" && act.UserRole != "owner":
		writeErr(w, http.StatusForbidden, "only an owner can remove an owner")
		return
	}
	if _, err := a.PG.Exec(r.Context(), `UPDATE users SET disabled_at = now() WHERE id = $1`, r.PathValue("user")); err != nil {
		writeDBErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "removed"})
}

var statusRe = regexp.MustCompile(`^[a-z_]{1,20}$`)

// chString quotes a value for a ClickHouse string literal.
func chString(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
}

// searchMessages lists an account's messages, newest first, from ClickHouse.
// Paging: pass the last row's accepted_at and id as before / before_id.
func (a *API) searchMessages(w http.ResponseWriter, r *http.Request, act actor) {
	id, ok := a.managedOrSelf(w, r, act)
	if !ok {
		return
	}
	if a.CH == nil {
		writeErr(w, http.StatusServiceUnavailable, "message search needs CLICKHOUSE_URL")
		return
	}
	q := r.URL.Query()
	from, to, err := period(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if q.Get("from") == "" {
		from = time.Now().Add(-24 * time.Hour)
	}
	conds := []string{fmt.Sprintf("account_id = toUUID('%s')", id), "accepted_at >= " + chTime(from), "accepted_at < " + chTime(to)}
	if v := q.Get("status"); v != "" {
		if !statusRe.MatchString(v) {
			writeErr(w, http.StatusBadRequest, "bad status")
			return
		}
		conds = append(conds, "status = "+chString(v))
	}
	if v := q.Get("to_number"); v != "" {
		if d, err := numbering.NormalizeIndia(v); err == nil {
			conds = append(conds, "destination = "+chString(d.E164))
		} else {
			conds = append(conds, "endsWith(destination, "+chString(strings.TrimLeft(v, "+"))+")")
		}
	}
	for param, col := range map[string]string{"sender": "sender", "client_ref": "client_ref", "template_id": "template_id", "vendor": "vendor_id"} {
		if v := q.Get(param); v != "" {
			conds = append(conds, col+" = "+chString(v))
		}
	}
	if v := q.Get("message_id"); v != "" {
		if _, err := uuid.Parse(v); err != nil {
			writeErr(w, http.StatusBadRequest, "bad message_id")
			return
		}
		conds = append(conds, "message_id = toUUID("+chString(v)+")")
	}
	if b := q.Get("before"); b != "" {
		bt, err := time.Parse(time.RFC3339Nano, b)
		bid := q.Get("before_id")
		if _, e := uuid.Parse(bid); err != nil || e != nil {
			writeErr(w, http.StatusBadRequest, "before (RFC 3339) and before_id (message ID) go together")
			return
		}
		conds = append(conds, fmt.Sprintf("(accepted_at, message_id) < (%s, toUUID(%s))", chTime(bt), chString(bid)))
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	// The account was checked (UUID from the database); every other value is
	// validated or quoted above.
	rows, err := a.CH.Query(r.Context(), fmt.Sprintf(`SELECT toString(message_id), formatDateTime(accepted_at, '%%Y-%%m-%%dT%%H:%%i:%%S.%%fZ', 'UTC'),
		destination, sender, segments, status, error_code, vendor_id, template_id, client_ref, toString(charges['%[1]s']), channel
		FROM message_records FINAL WHERE %[2]s ORDER BY accepted_at DESC, message_id DESC LIMIT %[3]d`,
		id, strings.Join(conds, " AND "), limit))
	if err != nil {
		a.Log.Error("message search", "err", err)
		writeErr(w, http.StatusServiceUnavailable, "message store unavailable")
		return
	}
	keys := []string{"id", "accepted_at", "to", "sender", "segments", "status", "error_code", "vendor_id", "template_id", "client_ref", "charged", "channel"}
	out := []map[string]string{}
	for _, row := range rows {
		if len(row) != len(keys) {
			continue
		}
		m := map[string]string{}
		for i, k := range keys {
			m[k] = row[i]
		}
		out = append(out, m)
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": out, "more": len(out) == limit})
}

// getMessage shows one message: live state from Redis while it is there
// (96 h), else the record from ClickHouse.
func (a *API) getMessage(w http.ResponseWriter, r *http.Request, act actor) {
	id, ok := a.managedOrSelf(w, r, act)
	if !ok {
		return
	}
	msg := r.PathValue("msg")
	if _, err := uuid.Parse(msg); err != nil {
		writeErr(w, http.StatusBadRequest, "bad message id")
		return
	}
	if a.Redis != nil {
		st, err := msgstore.New(a.Redis).Get(r.Context(), msg)
		if err == nil && st.AccountID == id {
			writeJSON(w, http.StatusOK, map[string]any{"source": "live", "message": map[string]any{
				"id": st.ID, "status": st.Status, "error_code": st.ErrorCode, "to": st.Destination, "sender": st.Sender,
				"segments": st.Segments, "price": st.Price, "vendor_id": st.VendorID, "vendor_msg_id": st.VendorMsgID,
				"pe_id": st.PEID, "template_id": st.TemplateID, "dlt_category": st.DLTCategory, "mcc": st.MCC, "mnc": st.MNC,
				"client_ref": st.ClientRef, "channel": st.Channel, "accepted_at": st.AcceptedAt, "done_at": st.DoneAt,
				"receipt_sent": st.ReceiptSent, "charges": st.Charges}})
			return
		}
	}
	if a.CH == nil {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	rows, err := a.CH.Query(r.Context(), fmt.Sprintf(`SELECT status, error_code, destination, sender, segments, toString(price), vendor_id,
		vendor_msg_id, pe_id, template_id, dlt_category, mcc, mnc, client_ref, channel, toString(accepted_at), toString(done_at)
		FROM message_records FINAL WHERE account_id = toUUID('%s') AND message_id = toUUID(%s) LIMIT 1`, id, chString(msg)))
	if err != nil || len(rows) == 0 || len(rows[0]) != 17 {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	keys := []string{"status", "error_code", "to", "sender", "segments", "price", "vendor_id", "vendor_msg_id", "pe_id", "template_id",
		"dlt_category", "mcc", "mnc", "client_ref", "channel", "accepted_at", "done_at"}
	m := map[string]string{"id": msg}
	for i, k := range keys {
		m[k] = rows[0][i]
	}
	writeJSON(w, http.StatusOK, map[string]any{"source": "records", "message": m})
}

// dashboard: wallet, today's traffic (IST) and the last 24 hours by hour.
func (a *API) dashboard(w http.ResponseWriter, r *http.Request, act actor) {
	id, ok := a.managedOrSelf(w, r, act)
	if !ok {
		return
	}
	out := map[string]any{}
	var bal, limit decimal.Decimal
	if err := a.PG.QueryRow(r.Context(), `SELECT balance, credit_limit FROM wallets WHERE account_id = $1`, id).Scan(&bal, &limit); err != nil {
		writeDBErr(w, err)
		return
	}
	wal := map[string]string{"balance": bal.StringFixed(2), "credit_limit": limit.StringFixed(2)}
	if a.Wallets != nil {
		if s, err := a.Wallets.Get(r.Context(), id); err == nil {
			wal["available"], wal["held"] = s.Available().Decimal().StringFixed(2), s.Held.Decimal().StringFixed(2)
			wal["credit_used"] = s.CreditUsed().Decimal().StringFixed(2)
			wal["live_balance"] = s.Balance.Decimal().StringFixed(2)
		}
	}
	out["wallet"] = wal
	var firing bool
	var threshold *decimal.Decimal
	if err := a.PG.QueryRow(r.Context(), `SELECT firing, low_balance FROM alert_settings WHERE account_id = $1 AND enabled`, id).
		Scan(&firing, &threshold); err == nil {
		out["low_balance_alert"] = map[string]any{"firing": firing, "threshold": threshold.StringFixed(2)}
	}
	if a.CH != nil {
		ist := time.FixedZone("IST", 5*3600+1800)
		now := time.Now().In(ist)
		midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, ist)
		rows, err := a.CH.Query(r.Context(), fmt.Sprintf(`SELECT status, count(), sum(segments), toString(sum(charges['%[1]s']))
			FROM message_records FINAL WHERE account_id = toUUID('%[1]s') AND accepted_at >= %[2]s
			GROUP BY status`, id, chTime(midnight)))
		if err == nil {
			today := map[string]any{}
			total, charged := 0, decimal.Zero
			for _, row := range rows {
				if len(row) == 4 {
					n, _ := strconv.Atoi(row[1])
					c, _ := decimal.NewFromString(row[3])
					today[row[0]] = n
					total += n
					charged = charged.Add(c)
				}
			}
			today["total"], today["charged"] = total, charged.StringFixed(2)
			out["today"] = today
		}
		rows, err = a.CH.Query(r.Context(), fmt.Sprintf(`SELECT formatDateTime(toStartOfHour(accepted_at), '%%Y-%%m-%%dT%%H:00:00', 'Asia/Kolkata'),
			count(), countIf(status = 'delivered'), countIf(status IN ('failed', 'expired', 'rejected'))
			FROM message_records FINAL WHERE account_id = toUUID('%s') AND accepted_at >= %s GROUP BY 1 ORDER BY 1`,
			id, chTime(time.Now().Add(-24*time.Hour))))
		if err == nil {
			series := []map[string]string{}
			for _, row := range rows {
				if len(row) == 4 {
					series = append(series, map[string]string{"hour": row[0], "sent": row[1], "delivered": row[2], "failed": row[3]})
				}
			}
			out["last_24h"] = series
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// traffic: messages per minute for the last ?minutes (default 60, max 1440).
func (a *API) traffic(w http.ResponseWriter, r *http.Request, act actor) {
	id, ok := a.managedOrSelf(w, r, act)
	if !ok {
		return
	}
	if a.CH == nil {
		writeErr(w, http.StatusServiceUnavailable, "traffic needs CLICKHOUSE_URL")
		return
	}
	mins, _ := strconv.Atoi(r.URL.Query().Get("minutes"))
	if mins <= 0 || mins > 1440 {
		mins = 60
	}
	rows, err := a.CH.Query(r.Context(), fmt.Sprintf(`SELECT formatDateTime(toStartOfMinute(accepted_at), '%%Y-%%m-%%dT%%H:%%i:00Z', 'UTC'),
		count(), countIf(status = 'delivered'), countIf(status IN ('failed', 'expired', 'rejected'))
		FROM message_records FINAL WHERE account_id = toUUID('%s') AND accepted_at >= %s GROUP BY 1 ORDER BY 1`,
		id, chTime(time.Now().Add(-time.Duration(mins)*time.Minute))))
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "message store unavailable")
		return
	}
	out := []map[string]string{}
	for _, row := range rows {
		if len(row) == 4 {
			out = append(out, map[string]string{"minute": row[0], "sent": row[1], "delivered": row[2], "failed": row[3]})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

type alertSettings struct {
	LowBalance decimal.Decimal `json:"low_balance"`
	Emails     []string        `json:"emails"`
	SMSNumbers []string        `json:"sms_numbers"`
	WebhookURL string          `json:"webhook_url"`
	Enabled    bool            `json:"enabled"`
}

func (a *API) getAlerts(w http.ResponseWriter, r *http.Request, act actor) {
	id, ok := a.managedOrSelf(w, r, act)
	if !ok {
		return
	}
	var s alertSettings
	var firing bool
	var last *time.Time
	err := a.PG.QueryRow(r.Context(), `SELECT low_balance, emails, sms_numbers, webhook_url, enabled, firing, last_fired_at
		FROM alert_settings WHERE account_id = $1`, id).Scan(&s.LowBalance, &s.Emails, &s.SMSNumbers, &s.WebhookURL, &s.Enabled, &firing, &last)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		writeDBErr(w, err)
		return
	}
	found := err == nil
	events := []map[string]any{}
	rows, err := a.PG.Query(r.Context(), `SELECT kind, available, threshold, delivered, errors, created_at FROM alert_events
		WHERE account_id = $1 ORDER BY created_at DESC LIMIT 20`, id)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var kind, errs string
			var avail, thr decimal.Decimal
			var delivered []string
			var at time.Time
			if rows.Scan(&kind, &avail, &thr, &delivered, &errs, &at) == nil {
				events = append(events, map[string]any{"kind": kind, "available": avail.StringFixed(2), "threshold": thr.StringFixed(2),
					"delivered": delivered, "errors": errs, "at": at})
			}
		}
	}
	if !found {
		writeJSON(w, http.StatusOK, map[string]any{"settings": nil, "events": events})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"settings": s, "firing": firing, "last_fired_at": last, "events": events})
}

func (a *API) putAlerts(w http.ResponseWriter, r *http.Request, act actor) {
	id, ok := a.managedOrSelf(w, r, act)
	if !ok {
		return
	}
	var s alertSettings
	if !decode(w, r, &s) {
		return
	}
	if s.LowBalance.IsNegative() {
		writeErr(w, http.StatusBadRequest, "low_balance must be >= 0")
		return
	}
	for _, e := range s.Emails {
		if !emailRe.MatchString(e) {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("%q is not an e-mail address", e))
			return
		}
	}
	nums := make([]string, 0, len(s.SMSNumbers))
	for _, n := range s.SMSNumbers {
		d, err := numbering.NormalizeIndia(n)
		if err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("%q is not an Indian mobile number", n))
			return
		}
		nums = append(nums, d.E164)
	}
	if s.WebhookURL != "" {
		if u, err := url.Parse(s.WebhookURL); err != nil || u.Scheme != "https" || u.Host == "" {
			writeErr(w, http.StatusBadRequest, "webhook_url must be an https URL")
			return
		}
	}
	if len(s.Emails)+len(nums) == 0 && s.WebhookURL == "" && s.Enabled {
		writeErr(w, http.StatusBadRequest, "add at least one e-mail, SMS number or webhook")
		return
	}
	if s.Emails == nil {
		s.Emails = []string{}
	}
	_, err := a.PG.Exec(r.Context(), `INSERT INTO alert_settings (account_id, low_balance, emails, sms_numbers, webhook_url, enabled, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (account_id) DO UPDATE SET low_balance = EXCLUDED.low_balance, emails = EXCLUDED.emails,
		  sms_numbers = EXCLUDED.sms_numbers, webhook_url = EXCLUDED.webhook_url, enabled = EXCLUDED.enabled,
		  updated_by = EXCLUDED.updated_by, updated_at = now()`,
		id, s.LowBalance, s.Emails, nums, s.WebhookURL, s.Enabled, act.User)
	if err != nil {
		writeDBErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "saved", "note": "checked every 30 seconds"})
}

// monitorLive answers the platform's live view from Prometheus: rates over
// the last 30 minutes and current vendor state.
func (a *API) monitorLive(w http.ResponseWriter, r *http.Request, act actor) {
	if !platformOnly(w, act) {
		return
	}
	if a.PromURL == "" {
		writeErr(w, http.StatusServiceUnavailable, "live monitoring needs PROMETHEUS_URL")
		return
	}
	ranges := map[string]string{
		"accepted_tps":      `sum(rate(sms_accepted_total[1m]))`,
		"refused_tps":       `sum(rate(sms_refused_total[1m]))`,
		"delivered_tps":     `sum(rate(sms_final_total{status="delivered"}[1m]))`,
		"failed_tps":        `sum(rate(sms_final_total{status=~"failed|expired|rejected"}[1m]))`,
		"accept_p99_ms":     `1000 * histogram_quantile(0.99, sum by (le) (rate(sms_accept_seconds_bucket[1m])))`,
		"vendor_submit_tps": `sum by (vendor) (rate(sms_vendor_submits_total[1m]))`,
	}
	instants := map[string]string{
		"refused_by_reason": `sum by (reason) (rate(sms_refused_total[5m]))`,
		"binds_up":          `sum by (vendor) (sms_vendor_binds_up)`,
		"routable":          `min by (vendor) (sms_vendor_routable)`,
		"customer_binds":    `sum(sms_customer_binds)`,
		"rerouted":          `sum by (from_vendor, reason) (increase(sms_rerouted_total[15m]))`,
	}
	end := time.Now()
	start := end.Add(-30 * time.Minute)
	out := map[string]any{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	var firstErr error
	run := func(name, path string, v url.Values) {
		defer wg.Done()
		res, err := a.promQuery(r.Context(), path, v)
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			return
		}
		out[name] = res
	}
	for name, q := range ranges {
		wg.Add(1)
		go run(name, "/api/v1/query_range", url.Values{"query": {q}, "start": {strconv.FormatInt(start.Unix(), 10)},
			"end": {strconv.FormatInt(end.Unix(), 10)}, "step": {"15"}})
	}
	for name, q := range instants {
		wg.Add(1)
		go run(name, "/api/v1/query", url.Values{"query": {q}})
	}
	wg.Wait()
	if len(out) == 0 && firstErr != nil {
		a.Log.Error("prometheus", "err", firstErr)
		writeErr(w, http.StatusServiceUnavailable, "monitoring store unavailable")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// promSeries is one Prometheus result: labels and [time, value] points.
type promSeries struct {
	Labels map[string]string `json:"labels"`
	Points [][2]float64      `json:"points"`
}

func (a *API) promQuery(ctx context.Context, path string, v url.Values) ([]promSeries, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(a.PromURL, "/")+path+"?"+v.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	var pr struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
				Value  []any             `json:"value"`
				Values [][]any           `json:"values"`
			} `json:"result"`
		} `json:"data"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &pr); err != nil || pr.Status != "success" {
		return nil, fmt.Errorf("prometheus: %s %s", resp.Status, pr.Error)
	}
	point := func(p []any) ([2]float64, bool) {
		if len(p) != 2 {
			return [2]float64{}, false
		}
		t, _ := p[0].(float64)
		s, _ := p[1].(string)
		f, err := strconv.ParseFloat(s, 64)
		if err != nil || f != f { // NaN (no traffic): leave out
			return [2]float64{}, false
		}
		return [2]float64{t, f}, true
	}
	out := make([]promSeries, 0, len(pr.Data.Result))
	for _, res := range pr.Data.Result {
		s := promSeries{Labels: res.Metric}
		if res.Value != nil {
			if p, ok := point(res.Value); ok {
				s.Points = append(s.Points, p)
			}
		}
		for _, v := range res.Values {
			if p, ok := point(v); ok {
				s.Points = append(s.Points, p)
			}
		}
		out = append(out, s)
	}
	return out, nil
}
