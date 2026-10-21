package adminapi

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/jpamaze/amazeA2pSMS/internal/billing/ledger"
	"github.com/jpamaze/amazeA2pSMS/internal/billing/recon"
	"github.com/jpamaze/amazeA2pSMS/internal/billing/wallet"
)

// ---- Customer disputes -------------------------------------------------
//
// An account claims it was wrongly charged; its parent (the platform, or the
// reseller that owns it) reviews the evidence and approves a credit, paid by
// the parent: platform credits are refunds, reseller credits come out of the
// reseller's wallet.

type disputeReq struct {
	From          time.Time       `json:"from"`
	To            time.Time       `json:"to"`
	MessageIDs    []string        `json:"message_ids"`
	ClaimedAmount decimal.Decimal `json:"claimed_amount"`
	Reason        string          `json:"reason"`
}

func (a *API) raiseDispute(w http.ResponseWriter, r *http.Request, act actor) {
	id := r.PathValue("id")
	if id != act.AccountID || act.Role == "platform" {
		writeErr(w, http.StatusForbidden, "an account raises disputes for itself; the platform resolves them")
		return
	}
	var req disputeReq
	if !decode(w, r, &req) {
		return
	}
	switch {
	case !req.ClaimedAmount.IsPositive():
		writeErr(w, http.StatusBadRequest, "claimed_amount must be > 0")
		return
	case req.From.IsZero() || !req.To.After(req.From):
		writeErr(w, http.StatusBadRequest, "from and to (RFC 3339) are required; to must be after from")
		return
	case strings.TrimSpace(req.Reason) == "":
		writeErr(w, http.StatusBadRequest, "reason is required")
		return
	case len(req.MessageIDs) > 1000:
		writeErr(w, http.StatusBadRequest, "at most 1,000 message_ids; describe larger claims by period")
		return
	}
	for _, m := range req.MessageIDs {
		if _, err := uuid.Parse(m); err != nil {
			writeErr(w, http.StatusBadRequest, "message_ids must be message IDs (UUIDs)")
			return
		}
	}
	if req.MessageIDs == nil {
		req.MessageIDs = []string{}
	}
	var did string
	err := a.PG.QueryRow(r.Context(), `INSERT INTO customer_disputes
		(account_id, resolver_id, period_from, period_to, message_ids, claimed_amount, reason, raised_by)
		SELECT id, parent_id, $2, $3, $4, $5, $6, $7 FROM accounts WHERE id = $1 RETURNING id`,
		id, req.From, req.To, req.MessageIDs, req.ClaimedAmount, req.Reason, act.User).Scan(&did)
	if err != nil {
		writeDBErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": did, "status": "open"})
}

type dispute struct {
	ID             string           `json:"id"`
	AccountID      string           `json:"account_id"`
	ResolverID     *string          `json:"resolver_id"`
	From           time.Time        `json:"from"`
	To             time.Time        `json:"to"`
	MessageIDs     []string         `json:"message_ids"`
	ClaimedAmount  decimal.Decimal  `json:"claimed_amount"`
	Reason         string           `json:"reason"`
	Status         string           `json:"status"`
	ApprovedAmount *decimal.Decimal `json:"approved_amount,omitempty"`
	Resolution     *string          `json:"resolution,omitempty"`
	RaisedBy       string           `json:"raised_by"`
	ResolvedBy     *string          `json:"resolved_by,omitempty"`
	CreatedAt      time.Time        `json:"created_at"`
	ResolvedAt     *time.Time       `json:"resolved_at,omitempty"`
}

const disputeCols = `id, account_id, resolver_id::text, period_from, period_to, message_ids::text[], claimed_amount, reason,
	status, approved_amount, resolution, raised_by, resolved_by, created_at, resolved_at`

func scanDispute(row pgx.Row) (dispute, error) {
	var d dispute
	err := row.Scan(&d.ID, &d.AccountID, &d.ResolverID, &d.From, &d.To, &d.MessageIDs, &d.ClaimedAmount, &d.Reason,
		&d.Status, &d.ApprovedAmount, &d.Resolution, &d.RaisedBy, &d.ResolvedBy, &d.CreatedAt, &d.ResolvedAt)
	return d, err
}

// resolver reports whether act resolves d: its parent account. A NULL
// resolver (an account with no parent) is the platform's.
func (d dispute) resolvedBy(act actor) bool {
	if d.ResolverID == nil {
		return act.Role == "platform"
	}
	return *d.ResolverID == act.AccountID
}

func (a *API) listDisputes(w http.ResponseWriter, r *http.Request, act actor) {
	status := r.URL.Query().Get("status")
	rows, err := a.PG.Query(r.Context(), `SELECT `+disputeCols+` FROM customer_disputes
		WHERE (account_id = $1 OR resolver_id = $1) AND ($2 = '' OR status = $2)
		ORDER BY created_at DESC LIMIT 500`, act.AccountID, status)
	if err != nil {
		writeDBErr(w, err)
		return
	}
	defer rows.Close()
	out := []dispute{}
	for rows.Next() {
		d, err := scanDispute(rows)
		if err != nil {
			writeDBErr(w, err)
			return
		}
		out = append(out, d)
	}
	writeJSON(w, http.StatusOK, out)
}

// loadDispute returns the path dispute if act raised or resolves it.
func (a *API) loadDispute(w http.ResponseWriter, r *http.Request, act actor) (dispute, bool) {
	d, err := scanDispute(a.PG.QueryRow(r.Context(), `SELECT `+disputeCols+` FROM customer_disputes WHERE id = $1`, r.PathValue("dispute")))
	if err == nil && d.AccountID != act.AccountID && !d.resolvedBy(act) {
		err = pgx.ErrNoRows
	}
	if err != nil {
		writeDBErr(w, err)
		return d, false
	}
	return d, true
}

// getDispute returns the dispute with evidence from our records: what the
// account was charged in the period by outcome, and each listed message.
func (a *API) getDispute(w http.ResponseWriter, r *http.Request, act actor) {
	d, ok := a.loadDispute(w, r, act)
	if !ok {
		return
	}
	out := map[string]any{"dispute": d}
	if a.CH == nil {
		out["evidence"] = "unavailable: set CLICKHOUSE_URL"
		writeJSON(w, http.StatusOK, out)
		return
	}
	// account_id and message IDs are UUIDs read from the database.
	rows, err := a.CH.Query(r.Context(), fmt.Sprintf(`SELECT status, count(), sum(segments), toString(sum(charges['%[1]s']))
		FROM message_records FINAL
		WHERE account_id = toUUID('%[1]s') AND accepted_at >= %[2]s AND accepted_at < %[3]s
		GROUP BY status ORDER BY status`, d.AccountID, chTime(d.From), chTime(d.To)))
	if err != nil {
		a.Log.Error("dispute evidence", "err", err)
		writeErr(w, http.StatusServiceUnavailable, "usage store unavailable")
		return
	}
	byStatus := []map[string]string{}
	for _, row := range rows {
		if len(row) == 4 {
			byStatus = append(byStatus, map[string]string{"status": row[0], "messages": row[1], "segments": row[2], "charged": row[3]})
		}
	}
	ev := map[string]any{"period_by_status": byStatus}
	if len(d.MessageIDs) > 0 {
		ids := "'" + strings.Join(d.MessageIDs, "','") + "'"
		rows, err := a.CH.Query(r.Context(), fmt.Sprintf(`SELECT toString(message_id), status, error_code, vendor_id, segments,
			toString(charges['%[1]s']), toString(accepted_at)
			FROM message_records FINAL WHERE account_id = toUUID('%[1]s') AND message_id IN (%[2]s)`, d.AccountID, ids))
		if err != nil {
			a.Log.Error("dispute evidence", "err", err)
			writeErr(w, http.StatusServiceUnavailable, "usage store unavailable")
			return
		}
		found := map[string]bool{}
		msgs := []map[string]string{}
		for _, row := range rows {
			if len(row) == 7 {
				found[row[0]] = true
				msgs = append(msgs, map[string]string{"message_id": row[0], "status": row[1], "error_code": row[2],
					"vendor_id": row[3], "segments": row[4], "charged": row[5], "accepted_at": row[6]})
			}
		}
		missing := []string{}
		for _, m := range d.MessageIDs {
			if !found[m] {
				missing = append(missing, m)
			}
		}
		ev["messages"], ev["not_found"] = msgs, missing
	}
	out["evidence"] = ev
	writeJSON(w, http.StatusOK, out)
}

func (a *API) resolveDispute(w http.ResponseWriter, r *http.Request, act actor) {
	d, ok := a.loadDispute(w, r, act)
	if !ok {
		return
	}
	if !d.resolvedBy(act) {
		writeErr(w, http.StatusForbidden, "disputes are resolved by the account's parent")
		return
	}
	var req struct {
		Decision string          `json:"decision"` // approve | reject
		Amount   decimal.Decimal `json:"amount"`   // approve: credited, at most the claim
		Note     string          `json:"note"`
	}
	if !decode(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Note) == "" {
		writeErr(w, http.StatusBadRequest, "note is required")
		return
	}
	switch req.Decision {
	case "reject":
		tag, err := a.PG.Exec(r.Context(), `UPDATE customer_disputes SET status = 'rejected', resolution = $2,
			resolved_by = $3, resolved_at = now() WHERE id = $1 AND status = 'open'`, d.ID, req.Note, act.User)
		if err != nil {
			writeDBErr(w, err)
			return
		}
		if tag.RowsAffected() == 0 {
			writeErr(w, http.StatusConflict, "dispute is not open")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "rejected"})
		return
	case "approve":
	default:
		writeErr(w, http.StatusBadRequest, "decision must be approve or reject")
		return
	}
	if !req.Amount.IsPositive() || req.Amount.GreaterThan(d.ClaimedAmount) {
		writeErr(w, http.StatusBadRequest, "amount must be > 0 and at most the claimed amount")
		return
	}
	// Claim the dispute first so two approvals cannot both pay; undo the
	// claim if the credit cannot be paid.
	tag, err := a.PG.Exec(r.Context(), `UPDATE customer_disputes SET status = 'approved', approved_amount = $2,
		resolution = $3, resolved_by = $4, resolved_at = now() WHERE id = $1 AND status = 'open'`, d.ID, req.Amount, req.Note, act.User)
	if err != nil {
		writeDBErr(w, err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeErr(w, http.StatusConflict, "dispute is not open")
		return
	}
	from := ""
	if d.ResolverID != nil && act.Role != "platform" {
		from = *d.ResolverID
	}
	bal, err := a.Ledger.Refund(r.Context(), ledger.Transfer{From: from, To: d.AccountID, Amount: req.Amount,
		Reference: "dispute " + d.ID, IdempotencyKey: "dispute:" + d.ID, Actor: act.User})
	if err != nil && !errors.Is(err, ledger.ErrDuplicate) {
		_, _ = a.PG.Exec(r.Context(), `UPDATE customer_disputes SET status = 'open', approved_amount = NULL,
			resolution = NULL, resolved_by = NULL, resolved_at = NULL WHERE id = $1`, d.ID)
		if errors.Is(err, wallet.ErrInsufficient) {
			writeErr(w, http.StatusUnprocessableEntity, "your available balance does not cover this credit")
			return
		}
		a.Log.Error("dispute credit", "dispute", d.ID, "err", err)
		writeErr(w, http.StatusInternalServerError, "credit failed; dispute left open")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "approved", "credited": req.Amount.StringFixed(4),
		"claimant_balance": bal.StringFixed(4)})
}

func (a *API) withdrawDispute(w http.ResponseWriter, r *http.Request, act actor) {
	tag, err := a.PG.Exec(r.Context(), `UPDATE customer_disputes SET status = 'withdrawn', resolved_by = $3, resolved_at = now()
		WHERE id = $1 AND account_id = $2 AND status = 'open'`, r.PathValue("dispute"), act.AccountID, act.User)
	if err != nil {
		writeDBErr(w, err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeErr(w, http.StatusNotFound, "no open dispute of yours with this id")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "withdrawn"})
}

// ---- Vendor invoices and disputes -------------------------------------

// importVendorInvoice stores what a vendor billed (CSV day,mcc,mnc,segments,amount
// with ?reference=&from=&to=, days in IST) and returns the reconciliation.
func (a *API) importVendorInvoice(w http.ResponseWriter, r *http.Request, act actor) {
	if !platformOnly(w, act) {
		return
	}
	q := r.URL.Query()
	ref := strings.TrimSpace(q.Get("reference"))
	from, err1 := time.Parse(time.DateOnly, q.Get("from"))
	to, err2 := time.Parse(time.DateOnly, q.Get("to"))
	if ref == "" || err1 != nil || err2 != nil || to.Before(from) {
		writeErr(w, http.StatusBadRequest, "reference, from and to (YYYY-MM-DD, inclusive) are required")
		return
	}
	lines, err := readInvoiceCSV(io.LimitReader(r.Body, 16<<20), from, to)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var segs int64
	var amt decimal.Decimal
	for _, l := range lines {
		segs += l.Segments
		amt = amt.Add(l.Amount)
	}
	var id string
	err = pgx.BeginFunc(r.Context(), a.PG, func(tx pgx.Tx) error {
		if err := tx.QueryRow(r.Context(), `INSERT INTO vendor_invoices
			(vendor_id, reference, period_from, period_to, total_segments, total_amount, imported_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
			r.PathValue("vendor"), ref, from, to, segs, amt, act.User).Scan(&id); err != nil {
			return err
		}
		rows := make([][]any, len(lines))
		for i, l := range lines {
			rows[i] = []any{id, l.Day, l.MCC, l.MNC, l.Segments, l.Amount}
		}
		_, err := tx.CopyFrom(r.Context(), pgx.Identifier{"vendor_invoice_lines"},
			[]string{"invoice_id", "day", "mcc", "mnc", "segments", "amount"}, pgx.CopyFromRows(rows))
		return err
	})
	if err != nil {
		writeDBErr(w, err)
		return
	}
	res, ok := a.reconcile(w, r, id, decimal.RequireFromString("0.5"))
	if !ok {
		writeJSON(w, http.StatusCreated, map[string]any{"id": id, "lines": len(lines), "reconciliation": "unavailable"})
		return
	}
	res["id"] = id
	writeJSON(w, http.StatusCreated, res)
}

func readInvoiceCSV(body io.Reader, from, to time.Time) ([]recon.Line, error) {
	cr := csv.NewReader(body)
	cr.FieldsPerRecord = 5
	cr.TrimLeadingSpace = true
	recs, err := cr.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("CSV: %v", err)
	}
	if len(recs) > 0 && strings.EqualFold(recs[0][0], "day") {
		recs = recs[1:]
	}
	if len(recs) == 0 {
		return nil, errors.New("CSV has no lines (header: day,mcc,mnc,segments,amount)")
	}
	seen := map[string]bool{}
	out := make([]recon.Line, 0, len(recs))
	for i, rec := range recs {
		n := i + 2
		day, err := time.Parse(time.DateOnly, rec[0])
		if err != nil || day.Before(from) || day.After(to) {
			return nil, fmt.Errorf("line %d: day must be YYYY-MM-DD within the invoice period", n)
		}
		if err := validDest(rec[1], rec[2]); err != nil {
			return nil, fmt.Errorf("line %d: %v", n, err)
		}
		segs, err := strconv.ParseInt(rec[3], 10, 64)
		if err != nil || segs < 0 {
			return nil, fmt.Errorf("line %d: segments must be a whole number >= 0", n)
		}
		amt, err := decimal.NewFromString(rec[4])
		if err != nil || amt.IsNegative() {
			return nil, fmt.Errorf("line %d: amount must be a number >= 0", n)
		}
		k := rec[0] + "|" + rec[1] + "|" + rec[2]
		if seen[k] {
			return nil, fmt.Errorf("line %d: duplicate day/mcc/mnc", n)
		}
		seen[k] = true
		out = append(out, recon.Line{Day: rec[0], MCC: rec[1], MNC: rec[2], Segments: segs, Amount: amt})
	}
	return out, nil
}

func tolerance(r *http.Request) (decimal.Decimal, error) {
	s := r.URL.Query().Get("tolerance_pct")
	if s == "" {
		return decimal.RequireFromString("0.5"), nil
	}
	t, err := decimal.NewFromString(s)
	if err != nil || t.IsNegative() || t.GreaterThan(decimal.NewFromInt(100)) {
		return t, errors.New("tolerance_pct must be between 0 and 100")
	}
	return t, nil
}

// reconcile compares an invoice with our records. It writes the error
// response itself when it returns false.
func (a *API) reconcile(w http.ResponseWriter, r *http.Request, invoiceID string, tol decimal.Decimal) (map[string]any, bool) {
	var vendor, ref string
	var from, to time.Time
	err := a.PG.QueryRow(r.Context(), `SELECT vendor_id, reference, period_from, period_to FROM vendor_invoices WHERE id = $1`,
		invoiceID).Scan(&vendor, &ref, &from, &to)
	if err != nil {
		writeDBErr(w, err)
		return nil, false
	}
	rows, err := a.PG.Query(r.Context(), `SELECT to_char(day, 'YYYY-MM-DD'), mcc, mnc, segments, amount
		FROM vendor_invoice_lines WHERE invoice_id = $1`, invoiceID)
	if err != nil {
		writeDBErr(w, err)
		return nil, false
	}
	var theirs []recon.Line
	for rows.Next() {
		var l recon.Line
		if err := rows.Scan(&l.Day, &l.MCC, &l.MNC, &l.Segments, &l.Amount); err != nil {
			rows.Close()
			writeDBErr(w, err)
			return nil, false
		}
		theirs = append(theirs, l)
	}
	rows.Close()
	if a.CH == nil {
		writeErr(w, http.StatusServiceUnavailable, "reconciliation needs CLICKHOUSE_URL")
		return nil, false
	}
	// Invoice days are IST days.
	ist := time.FixedZone("IST", 5*3600+1800)
	start := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, ist)
	end := time.Date(to.Year(), to.Month(), to.Day(), 0, 0, 0, 0, ist).AddDate(0, 0, 1)
	res, err := a.CH.Query(r.Context(), fmt.Sprintf(`SELECT toString(toDate(accepted_at, 'Asia/Kolkata')) AS day, mcc, mnc,
		sum(segments), toString(sum(cost))
		FROM message_records FINAL
		WHERE vendor_id = '%s' AND %s AND accepted_at >= %s AND accepted_at < %s
		GROUP BY day, mcc, mnc`, vendor, vendorBilled, chTime(start), chTime(end)))
	if err != nil {
		a.Log.Error("reconciliation", "err", err)
		writeErr(w, http.StatusServiceUnavailable, "usage store unavailable")
		return nil, false
	}
	var ours []recon.Line
	for _, row := range res {
		if len(row) != 5 {
			continue
		}
		l := recon.Line{Day: row[0], MCC: row[1], MNC: row[2]}
		fmt.Sscan(row[3], &l.Segments)
		l.Amount, _ = decimal.NewFromString(row[4])
		ours = append(ours, l)
	}
	diffs, sum := recon.Compare(theirs, ours, tol)
	return map[string]any{"invoice_id": invoiceID, "vendor_id": vendor, "reference": ref,
		"tolerance_pct": tol.String(), "summary": sum, "lines": diffs}, true
}

func (a *API) reconciliation(w http.ResponseWriter, r *http.Request, act actor) {
	if !platformOnly(w, act) {
		return
	}
	tol, err := tolerance(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if res, ok := a.reconcile(w, r, r.PathValue("invoice"), tol); ok {
		writeJSON(w, http.StatusOK, res)
	}
}

// raiseVendorDispute opens a dispute for the invoice's lines outside
// tolerance, for the amount the vendor billed above our records.
func (a *API) raiseVendorDispute(w http.ResponseWriter, r *http.Request, act actor) {
	if !platformOnly(w, act) {
		return
	}
	tol, err := tolerance(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var req struct {
		Notes string `json:"notes"`
	}
	if !decode(w, r, &req) {
		return
	}
	res, ok := a.reconcile(w, r, r.PathValue("invoice"), tol)
	if !ok {
		return
	}
	sum := res["summary"].(recon.Summary)
	if !sum.Disputable.IsPositive() {
		writeErr(w, http.StatusUnprocessableEntity, "nothing to dispute: no overbilled lines outside tolerance")
		return
	}
	var bad []recon.Diff
	for _, d := range res["lines"].([]recon.Diff) {
		if d.Status == recon.Mismatch || d.Status == recon.NotInOurs {
			bad = append(bad, d)
		}
	}
	lines, _ := json.Marshal(bad)
	var id string
	err = a.PG.QueryRow(r.Context(), `INSERT INTO vendor_disputes (invoice_id, disputed_amount, lines, notes, created_by)
		VALUES ($1, $2, $3, $4, $5) RETURNING id`, r.PathValue("invoice"), sum.Disputable, lines, req.Notes, act.User).Scan(&id)
	if err != nil {
		writeDBErr(w, err) // 409 when one is already open for this invoice
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "status": "open",
		"disputed_amount": sum.Disputable.StringFixed(4), "lines": bad})
}

type vendorDispute struct {
	ID             string           `json:"id"`
	InvoiceID      string           `json:"invoice_id"`
	VendorID       string           `json:"vendor_id"`
	Reference      string           `json:"invoice_reference"`
	Status         string           `json:"status"`
	DisputedAmount decimal.Decimal  `json:"disputed_amount"`
	SettledAmount  *decimal.Decimal `json:"settled_amount,omitempty"`
	Lines          json.RawMessage  `json:"lines"`
	Notes          string           `json:"notes"`
	CreatedBy      string           `json:"created_by"`
	CreatedAt      time.Time        `json:"created_at"`
	UpdatedAt      time.Time        `json:"updated_at"`
}

func (a *API) listVendorDisputes(w http.ResponseWriter, r *http.Request, act actor) {
	if !platformOnly(w, act) {
		return
	}
	rows, err := a.PG.Query(r.Context(), `SELECT d.id, d.invoice_id, i.vendor_id, i.reference, d.status, d.disputed_amount,
		d.settled_amount, d.lines, d.notes, d.created_by, d.created_at, d.updated_at
		FROM vendor_disputes d JOIN vendor_invoices i ON i.id = d.invoice_id
		WHERE ($1 = '' OR d.status = $1) AND ($2 = '' OR i.vendor_id = $2)
		ORDER BY d.created_at DESC LIMIT 500`, r.URL.Query().Get("status"), r.URL.Query().Get("vendor"))
	if err != nil {
		writeDBErr(w, err)
		return
	}
	defer rows.Close()
	out := []vendorDispute{}
	for rows.Next() {
		var d vendorDispute
		if err := rows.Scan(&d.ID, &d.InvoiceID, &d.VendorID, &d.Reference, &d.Status, &d.DisputedAmount,
			&d.SettledAmount, &d.Lines, &d.Notes, &d.CreatedBy, &d.CreatedAt, &d.UpdatedAt); err != nil {
			writeDBErr(w, err)
			return
		}
		out = append(out, d)
	}
	writeJSON(w, http.StatusOK, out)
}

// vendorDisputeNext lists the statuses each status may move to.
var vendorDisputeNext = map[string][]string{
	"open":     {"sent", "accepted", "rejected"},
	"sent":     {"accepted", "rejected"},
	"accepted": {"settled"},
	"rejected": {"settled"}, // settled for less, or written off with 0
}

func (a *API) updateVendorDispute(w http.ResponseWriter, r *http.Request, act actor) {
	if !platformOnly(w, act) {
		return
	}
	var req struct {
		Status        string           `json:"status"`
		SettledAmount *decimal.Decimal `json:"settled_amount"` // with status settled: credit note received
		Note          string           `json:"note"`
	}
	if !decode(w, r, &req) {
		return
	}
	if (req.Status == "settled") != (req.SettledAmount != nil) || (req.SettledAmount != nil && req.SettledAmount.IsNegative()) {
		writeErr(w, http.StatusBadRequest, "settled_amount (>= 0) is required with status settled, and only then")
		return
	}
	var cur string
	var disputed decimal.Decimal
	if err := a.PG.QueryRow(r.Context(), `SELECT status, disputed_amount FROM vendor_disputes WHERE id = $1`,
		r.PathValue("dispute")).Scan(&cur, &disputed); err != nil {
		writeDBErr(w, err)
		return
	}
	allowed := false
	for _, s := range vendorDisputeNext[cur] {
		allowed = allowed || s == req.Status
	}
	if !allowed {
		writeErr(w, http.StatusConflict, fmt.Sprintf("cannot move from %s to %q (allowed: %v)", cur, req.Status, vendorDisputeNext[cur]))
		return
	}
	if req.SettledAmount != nil && req.SettledAmount.GreaterThan(disputed) {
		writeErr(w, http.StatusBadRequest, "settled_amount cannot exceed the disputed amount")
		return
	}
	note := ""
	if strings.TrimSpace(req.Note) != "" {
		note = fmt.Sprintf("%s %s (%s): %s\n", time.Now().UTC().Format(time.DateOnly), req.Status, act.User, req.Note)
	}
	tag, err := a.PG.Exec(r.Context(), `UPDATE vendor_disputes SET status = $3, settled_amount = coalesce($4, settled_amount),
		notes = notes || $5, updated_at = now() WHERE id = $1 AND status = $2`,
		r.PathValue("dispute"), cur, req.Status, req.SettledAmount, note)
	if err != nil {
		writeDBErr(w, err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeErr(w, http.StatusConflict, "dispute changed meanwhile; reload and retry")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": req.Status})
}
