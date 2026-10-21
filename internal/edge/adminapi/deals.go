package adminapi

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/jpamaze/amazeA2pSMS/internal/billing/deals"
)

// commercialRoutes registers deals and disputes (phase 2 slice 2).
func (a *API) commercialRoutes(mux *http.ServeMux) {
	mux.Handle("POST /v1/accounts/{id}/deals", a.authed(PermAdmin, a.createDeal))
	mux.Handle("GET /v1/accounts/{id}/deals", a.authed(PermRead, a.listDeals))
	mux.Handle("POST /v1/deals/{deal}/cancel", a.authed(PermAdmin, a.cancelDeal))

	mux.Handle("POST /v1/vendors/{vendor}/deals", a.authed(PermAdmin, a.createVendorDeal))
	mux.Handle("GET /v1/vendor-deals", a.authed(PermRead, a.listVendorDeals))
	mux.Handle("GET /v1/vendor-deals/{deal}", a.authed(PermRead, a.vendorDealReport))
	mux.Handle("POST /v1/vendor-deals/{deal}/cancel", a.authed(PermAdmin, a.cancelVendorDeal))

	mux.Handle("POST /v1/accounts/{id}/disputes", a.authed(PermFinance, a.raiseDispute))
	mux.Handle("GET /v1/disputes", a.authed(PermRead, a.listDisputes))
	mux.Handle("GET /v1/disputes/{dispute}", a.authed(PermRead, a.getDispute))
	mux.Handle("POST /v1/disputes/{dispute}/resolve", a.authed(PermFinance, a.resolveDispute))
	mux.Handle("POST /v1/disputes/{dispute}/withdraw", a.authed(PermFinance, a.withdrawDispute))

	mux.Handle("POST /v1/vendors/{vendor}/invoices", a.authed(PermFinance, a.importVendorInvoice))
	mux.Handle("GET /v1/vendor-invoices/{invoice}/reconciliation", a.authed(PermRead, a.reconciliation))
	mux.Handle("POST /v1/vendor-invoices/{invoice}/disputes", a.authed(PermFinance, a.raiseVendorDispute))
	mux.Handle("GET /v1/vendor-disputes", a.authed(PermRead, a.listVendorDisputes))
	mux.Handle("PATCH /v1/vendor-disputes/{dispute}", a.authed(PermFinance, a.updateVendorDispute))
}

var (
	mccRe = regexp.MustCompile(`^[0-9]{3}$`)
	mncRe = regexp.MustCompile(`^([0-9]{2,3}|\*)$`)
)

func validDest(mcc, mnc string) error {
	if !mccRe.MatchString(mcc) || !mncRe.MatchString(mnc) {
		return errors.New("mcc must be 3 digits and mnc 2-3 digits or '*'")
	}
	return nil
}

// platformOnly writes 403 unless the actor is the platform.
func platformOnly(w http.ResponseWriter, act actor) bool {
	if act.Role != "platform" {
		writeErr(w, http.StatusForbidden, "platform users only")
		return false
	}
	return true
}

// child checks that the path account is a direct child of the actor: only
// the account's parent sets its prices, so only the parent grants deals.
func (a *API) child(w http.ResponseWriter, r *http.Request, act actor) (string, bool) {
	id := r.PathValue("id")
	var parent *string
	err := a.PG.QueryRow(r.Context(), `SELECT parent_id::text FROM accounts WHERE id = $1`, id).Scan(&parent)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusBadRequest, "invalid account id")
		return "", false
	}
	if err != nil || parent == nil || *parent != act.AccountID {
		writeErr(w, http.StatusForbidden, "only the account's parent (platform or reseller) can do this")
		return "", false
	}
	return id, true
}

type dealReq struct {
	Name        string          `json:"name"`
	MCC         string          `json:"mcc"`
	MNC         string          `json:"mnc"`
	Price       decimal.Decimal `json:"price"`
	CapSegments *int64          `json:"cap_segments"`
	StartsAt    time.Time       `json:"starts_at"`
	EndsAt      time.Time       `json:"ends_at"`
}

func (a *API) createDeal(w http.ResponseWriter, r *http.Request, act actor) {
	id, ok := a.child(w, r, act)
	if !ok {
		return
	}
	var req dealReq
	if !decode(w, r, &req) {
		return
	}
	if req.MNC == "" {
		req.MNC = "*"
	}
	switch {
	case strings.TrimSpace(req.Name) == "":
		writeErr(w, http.StatusBadRequest, "name is required")
		return
	case validDest(req.MCC, req.MNC) != nil:
		writeErr(w, http.StatusBadRequest, validDest(req.MCC, req.MNC).Error())
		return
	case req.Price.IsNegative():
		writeErr(w, http.StatusBadRequest, "price must be >= 0")
		return
	case req.CapSegments != nil && *req.CapSegments <= 0:
		writeErr(w, http.StatusBadRequest, "cap_segments must be > 0 (omit for no cap)")
		return
	case req.StartsAt.IsZero() || !req.EndsAt.After(req.StartsAt) || !req.EndsAt.After(time.Now()):
		writeErr(w, http.StatusBadRequest, "starts_at and ends_at (RFC 3339) are required; ends_at must be after starts_at and in the future")
		return
	}
	var dealID string
	err := pgx.BeginFunc(r.Context(), a.PG, func(tx pgx.Tx) error {
		// Serialise deal changes per account so overlap checks hold.
		if _, err := tx.Exec(r.Context(), `SELECT 1 FROM accounts WHERE id = $1 FOR UPDATE`, id); err != nil {
			return err
		}
		var clash string
		err := tx.QueryRow(r.Context(), `SELECT name FROM customer_deals
			WHERE account_id = $1 AND mcc = $2 AND mnc = $3 AND status = 'active'
			  AND starts_at < $5 AND ends_at > $4 LIMIT 1`, id, req.MCC, req.MNC, req.StartsAt, req.EndsAt).Scan(&clash)
		if err == nil {
			return fmt.Errorf("%w: overlaps deal %q on the same destination", errConflict, clash)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		return tx.QueryRow(r.Context(), `INSERT INTO customer_deals
			(account_id, granted_by, name, mcc, mnc, price, cap_segments, starts_at, ends_at, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING id`,
			id, act.AccountID, req.Name, req.MCC, req.MNC, req.Price, req.CapSegments, req.StartsAt, req.EndsAt, act.User).Scan(&dealID)
	})
	if errors.Is(err, errConflict) {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		writeDBErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": dealID})
}

var errConflict = errors.New("conflict")

func (a *API) listDeals(w http.ResponseWriter, r *http.Request, act actor) {
	id, ok := a.managedOrSelf(w, r, act)
	if !ok {
		return
	}
	rows, err := a.PG.Query(r.Context(), `SELECT id, name, mcc, mnc, price, cap_segments, used_segments,
		starts_at, ends_at, status, created_by FROM customer_deals WHERE account_id = $1 ORDER BY starts_at DESC LIMIT 500`, id)
	if err != nil {
		writeDBErr(w, err)
		return
	}
	defer rows.Close()
	type deal struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		MCC         string `json:"mcc"`
		MNC         string `json:"mnc"`
		Price       string `json:"price"`
		CapSegments *int64 `json:"cap_segments"`
		Charged     int64  `json:"charged_segments"`
		Reserved    *int64 `json:"live_used_segments,omitempty"` // charged plus in flight (capped deals)
		Remaining   *int64 `json:"remaining_segments,omitempty"`
		StartsAt    string `json:"starts_at"`
		EndsAt      string `json:"ends_at"`
		State       string `json:"state"`
		CreatedBy   string `json:"created_by"`
	}
	var out []deal
	var capped []string
	now := time.Now()
	for rows.Next() {
		var d deal
		var price decimal.Decimal
		var starts, ends time.Time
		var status string
		if err := rows.Scan(&d.ID, &d.Name, &d.MCC, &d.MNC, &price, &d.CapSegments, &d.Charged, &starts, &ends, &status, &d.CreatedBy); err != nil {
			writeDBErr(w, err)
			return
		}
		d.Price, d.StartsAt, d.EndsAt = price.StringFixed(4), starts.UTC().Format(time.RFC3339), ends.UTC().Format(time.RFC3339)
		d.State = windowState(status, starts, ends, now)
		if d.CapSegments != nil {
			capped = append(capped, d.ID)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		writeDBErr(w, err)
		return
	}
	var live map[string]int64
	if a.Redis != nil {
		live, _ = deals.Used(r.Context(), a.Redis, capped)
	}
	for i := range out {
		d := &out[i]
		if d.CapSegments == nil {
			continue
		}
		used := d.Charged
		if v, ok := live[d.ID]; ok {
			d.Reserved = &v
			used = max(used, v)
		}
		left := max(*d.CapSegments-used, 0)
		d.Remaining = &left
	}
	if out == nil {
		out = []deal{}
	}
	writeJSON(w, http.StatusOK, out)
}

// windowState: scheduled | running | ended | cancelled.
func windowState(status string, starts, ends, now time.Time) string {
	switch {
	case status == "cancelled":
		return "cancelled"
	case now.Before(starts):
		return "scheduled"
	case now.Before(ends):
		return "running"
	}
	return "ended"
}

func (a *API) cancelDeal(w http.ResponseWriter, r *http.Request, act actor) {
	tag, err := a.PG.Exec(r.Context(), `UPDATE customer_deals SET status = 'cancelled'
		WHERE id = $1 AND status = 'active' AND granted_by = $2`, r.PathValue("deal"), act.AccountID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid deal id")
		return
	}
	if tag.RowsAffected() == 0 {
		writeErr(w, http.StatusNotFound, "no active deal you granted with this id")
		return
	}
	// Edges cache deals for up to a minute.
	writeJSON(w, http.StatusOK, map[string]string{"status": "cancelled", "note": "stops applying within one minute"})
}

type vendorDealReq struct {
	Name           string          `json:"name"`
	MCC            string          `json:"mcc"`
	MNC            string          `json:"mnc"`
	Cost           decimal.Decimal `json:"cost"`
	CommitSegments int64           `json:"commit_segments"`
	ShortfallRate  decimal.Decimal `json:"shortfall_rate"`
	StartsAt       time.Time       `json:"starts_at"`
	EndsAt         time.Time       `json:"ends_at"`
}

func (a *API) createVendorDeal(w http.ResponseWriter, r *http.Request, act actor) {
	if !platformOnly(w, act) {
		return
	}
	var req vendorDealReq
	if !decode(w, r, &req) {
		return
	}
	if req.MNC == "" {
		req.MNC = "*"
	}
	switch {
	case strings.TrimSpace(req.Name) == "":
		writeErr(w, http.StatusBadRequest, "name is required")
		return
	case validDest(req.MCC, req.MNC) != nil:
		writeErr(w, http.StatusBadRequest, validDest(req.MCC, req.MNC).Error())
		return
	case req.Cost.IsNegative() || req.ShortfallRate.IsNegative():
		writeErr(w, http.StatusBadRequest, "cost and shortfall_rate must be >= 0")
		return
	case req.CommitSegments <= 0:
		writeErr(w, http.StatusBadRequest, "commit_segments must be > 0")
		return
	case req.StartsAt.IsZero() || !req.EndsAt.After(req.StartsAt):
		writeErr(w, http.StatusBadRequest, "starts_at and ends_at (RFC 3339) are required; ends_at must be after starts_at")
		return
	}
	var id string
	err := a.PG.QueryRow(r.Context(), `INSERT INTO vendor_deals
		(vendor_id, name, mcc, mnc, cost, commit_segments, shortfall_rate, starts_at, ends_at, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING id`,
		r.PathValue("vendor"), req.Name, req.MCC, req.MNC, req.Cost, req.CommitSegments, req.ShortfallRate,
		req.StartsAt, req.EndsAt, act.User).Scan(&id)
	if err != nil {
		writeDBErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": id, "note": "routing picks it up within 15 seconds"})
}

type vendorDeal struct {
	ID             string          `json:"id"`
	VendorID       string          `json:"vendor_id"`
	Name           string          `json:"name"`
	MCC            string          `json:"mcc"`
	MNC            string          `json:"mnc"`
	Cost           decimal.Decimal `json:"cost"`
	CommitSegments int64           `json:"commit_segments"`
	ShortfallRate  decimal.Decimal `json:"shortfall_rate"`
	StartsAt       time.Time       `json:"starts_at"`
	EndsAt         time.Time       `json:"ends_at"`
	Status         string          `json:"-"`
	State          string          `json:"state"`
	Routed         int64           `json:"routed_segments"` // live, from the routers
}

const vendorDealCols = `id, vendor_id, name, mcc, mnc, cost, commit_segments, shortfall_rate, starts_at, ends_at, status`

func scanVendorDeal(row pgx.Row) (vendorDeal, error) {
	var d vendorDeal
	err := row.Scan(&d.ID, &d.VendorID, &d.Name, &d.MCC, &d.MNC, &d.Cost, &d.CommitSegments, &d.ShortfallRate,
		&d.StartsAt, &d.EndsAt, &d.Status)
	d.State = windowState(d.Status, d.StartsAt, d.EndsAt, time.Now())
	return d, err
}

func (a *API) listVendorDeals(w http.ResponseWriter, r *http.Request, act actor) {
	if !platformOnly(w, act) {
		return
	}
	rows, err := a.PG.Query(r.Context(), `SELECT `+vendorDealCols+` FROM vendor_deals ORDER BY starts_at DESC LIMIT 500`)
	if err != nil {
		writeDBErr(w, err)
		return
	}
	defer rows.Close()
	out := []vendorDeal{}
	var ids []string
	for rows.Next() {
		d, err := scanVendorDeal(rows)
		if err != nil {
			writeDBErr(w, err)
			return
		}
		out = append(out, d)
		ids = append(ids, d.ID)
	}
	if a.Redis != nil {
		if n, err := deals.Routed(r.Context(), a.Redis, ids); err == nil {
			for i := range out {
				out[i].Routed = n[out[i].ID]
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) cancelVendorDeal(w http.ResponseWriter, r *http.Request, act actor) {
	if !platformOnly(w, act) {
		return
	}
	tag, err := a.PG.Exec(r.Context(), `UPDATE vendor_deals SET status = 'cancelled' WHERE id = $1 AND status = 'active'`, r.PathValue("deal"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid deal id")
		return
	}
	if tag.RowsAffected() == 0 {
		writeErr(w, http.StatusNotFound, "no active vendor deal with this id")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "cancelled"})
}

// vendorBilled matches records the vendor accepted, i.e. traffic it bills.
const vendorBilled = `vendor_id != '' AND (status IN ('submitted', 'delivered', 'expired') OR (status = 'failed' AND vendor_msg_id != ''))`

// chTime is a ClickHouse literal for t (accepted_at is stored in IST).
func chTime(t time.Time) string {
	return fmt.Sprintf("toDateTime64('%s', 3, 'UTC')", t.UTC().Format("2006-01-02 15:04:05.000"))
}

// vendorDealReport shows progress against the commitment: billed volume
// from ClickHouse, the run rate needed, a straight-line projection and the
// shortfall charge if the projection holds.
func (a *API) vendorDealReport(w http.ResponseWriter, r *http.Request, act actor) {
	if !platformOnly(w, act) {
		return
	}
	d, err := scanVendorDeal(a.PG.QueryRow(r.Context(), `SELECT `+vendorDealCols+` FROM vendor_deals WHERE id = $1`, r.PathValue("deal")))
	if err != nil {
		writeDBErr(w, err)
		return
	}
	if a.Redis != nil {
		if n, err := deals.Routed(r.Context(), a.Redis, []string{d.ID}); err == nil {
			d.Routed = n[d.ID]
		}
	}
	out := map[string]any{"deal": d}
	if a.CH == nil {
		out["billed"] = "unavailable: set CLICKHOUSE_URL"
		writeJSON(w, http.StatusOK, out)
		return
	}
	now := time.Now()
	until := d.EndsAt
	if now.Before(until) {
		until = now
	}
	var billed int64
	var cost decimal.Decimal
	if until.After(d.StartsAt) {
		mnc := ""
		if d.MNC != "*" {
			mnc = fmt.Sprintf(" AND mnc = '%s'", d.MNC)
		}
		// Values come from the database and were validated on the way in.
		rows, err := a.CH.Query(r.Context(), fmt.Sprintf(`SELECT sum(segments), toString(sum(cost)) FROM message_records FINAL
			WHERE vendor_id = '%s' AND mcc = '%s'%s AND %s AND accepted_at >= %s AND accepted_at < %s`,
			d.VendorID, d.MCC, mnc, vendorBilled, chTime(d.StartsAt), chTime(until)))
		if err != nil {
			a.Log.Error("vendor deal report", "err", err)
			writeErr(w, http.StatusServiceUnavailable, "usage store unavailable")
			return
		}
		if len(rows) == 1 && len(rows[0]) == 2 {
			fmt.Sscan(rows[0][0], &billed)
			cost, _ = decimal.NewFromString(rows[0][1])
		}
	}
	p := commitProgress(d.CommitSegments, billed, d.StartsAt, d.EndsAt, now)
	p["billed_segments"] = billed
	p["billed_cost"] = cost.StringFixed(4)
	if short, ok := p["projected_shortfall"].(int64); ok {
		p["shortfall_charge_projected"] = d.ShortfallRate.Mul(decimal.NewFromInt(short)).StringFixed(4)
	}
	out["progress"] = p
	writeJSON(w, http.StatusOK, out)
}

// commitProgress projects billed volume over the whole deal at the run rate
// so far. The projection needs a full day of history (or the deal's end) to
// mean anything, and is omitted before that.
func commitProgress(commit, billed int64, starts, ends, now time.Time) map[string]any {
	total := ends.Sub(starts).Hours() / 24
	elapsed := math.Min(math.Max(now.Sub(starts).Hours()/24, 0), total)
	left := total - elapsed
	remaining := max(commit-billed, 0)
	needed := int64(0)
	if left > 0 {
		needed = int64(math.Ceil(float64(remaining) / left))
	}
	p := map[string]any{
		"committed_segments": commit,
		"remaining_segments": remaining,
		"percent_met":        math.Round(float64(billed)/float64(commit)*1000) / 10,
		"days_total":         math.Round(total*10) / 10,
		"days_left":          math.Round(left*10) / 10,
		"needed_per_day":     needed,
	}
	if elapsed >= 1 || left <= 0 {
		projected := billed
		if left > 0 {
			projected += int64(float64(billed) / elapsed * left)
		}
		p["projected_segments"] = projected
		p["projected_shortfall"] = max(commit-projected, 0)
	}
	return p
}
