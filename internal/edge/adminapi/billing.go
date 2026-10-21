package adminapi

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/jpamaze/amazeA2pSMS/internal/billing/ledger"
	"github.com/jpamaze/amazeA2pSMS/internal/billing/wallet"
)

// billingRoutes registers phase 2 endpoints on mux.
func (a *API) billingRoutes(mux *http.ServeMux) {
	mux.Handle("POST /v1/rate-cards", a.authed(PermAdmin, a.createRateCard))
	mux.Handle("GET /v1/rate-cards/{card}/entries", a.authed(PermRead, a.listEntries))
	mux.Handle("PUT /v1/rate-cards/{card}/entries", a.authed(PermAdmin, a.putEntries))
	mux.Handle("POST /v1/rate-cards/{card}/entries/import", a.authed(PermAdmin, a.importEntries))
	mux.Handle("POST /v1/vendors/{vendor}/costs/import", a.authed(PermAdmin, a.importVendorCosts))
	mux.Handle("POST /v1/accounts/{id}/transfers", a.authed(PermFinance, a.transfer))
	mux.Handle("GET /v1/accounts/{id}/statement", a.authed(PermRead, a.statement))
	mux.Handle("GET /v1/accounts/{id}/usage", a.authed(PermRead, a.usage))
}

// cardOwner is the owner value for cards the actor creates and may manage:
// NULL (platform cards) for the platform, the reseller's own account otherwise.
func cardOwner(act actor) *string {
	if act.Role == "platform" {
		return nil
	}
	return &act.AccountID
}

// ownsCard reports whether the actor manages rate card id.
func (a *API) ownsCard(ctx context.Context, act actor, id string) (bool, error) {
	var owner *string
	err := a.PG.QueryRow(ctx, `SELECT owner_account_id::text FROM rate_cards WHERE id = $1`, id).Scan(&owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if act.Role == "platform" {
		return owner == nil, nil
	}
	return owner != nil && *owner == act.AccountID, nil
}

func (a *API) card(w http.ResponseWriter, r *http.Request, act actor) (string, bool) {
	id := r.PathValue("card")
	ok, err := a.ownsCard(r.Context(), act, id)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid rate card id")
		return "", false
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "no such rate card")
		return "", false
	}
	return id, true
}

func (a *API) createRateCard(w http.ResponseWriter, r *http.Request, act actor) {
	if act.Role == "customer" {
		writeErr(w, http.StatusForbidden, "customers cannot create rate cards")
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeErr(w, http.StatusBadRequest, "name is required")
		return
	}
	var id string
	if err := a.PG.QueryRow(r.Context(), `INSERT INTO rate_cards (name, owner_account_id) VALUES ($1, $2) RETURNING id`,
		req.Name, cardOwner(act)).Scan(&id); err != nil {
		writeDBErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": id, "name": req.Name})
}

type entry struct {
	MCC       string          `json:"mcc"`
	MNC       string          `json:"mnc"`
	Category  string          `json:"category"`
	Price     decimal.Decimal `json:"price"`
	ValidFrom time.Time       `json:"valid_from"`
}

func (e *entry) normalise(now time.Time) error {
	e.MCC, e.MNC, e.Category = strings.TrimSpace(e.MCC), strings.TrimSpace(e.MNC), strings.TrimSpace(e.Category)
	if e.MNC == "" {
		e.MNC = "*"
	}
	if e.Category == "" {
		e.Category = "*"
	}
	if len(e.MCC) != 3 || strings.Trim(e.MCC, "0123456789") != "" {
		return fmt.Errorf("mcc %q must be 3 digits", e.MCC)
	}
	if e.MNC != "*" && (len(e.MNC) < 2 || len(e.MNC) > 3 || strings.Trim(e.MNC, "0123456789") != "") {
		return fmt.Errorf("mnc %q must be 2-3 digits or *", e.MNC)
	}
	if e.Price.IsNegative() || e.Price.Exponent() < -4 {
		return fmt.Errorf("price %s must be >= 0 with at most 4 decimals", e.Price)
	}
	if e.ValidFrom.IsZero() {
		e.ValidFrom = now
	}
	return nil
}

// listEntries returns the price in force for each destination, plus
// scheduled future changes.
func (a *API) listEntries(w http.ResponseWriter, r *http.Request, act actor) {
	id, ok := a.card(w, r, act)
	if !ok {
		return
	}
	rows, err := a.PG.Query(r.Context(), `
		SELECT mcc, mnc, category, price, valid_from, valid_from > now() AS scheduled
		FROM rate_card_entries e
		WHERE rate_card_id = $1 AND (valid_from > now() OR valid_from = (
			SELECT max(valid_from) FROM rate_card_entries x
			WHERE x.rate_card_id = e.rate_card_id AND x.mcc = e.mcc AND x.mnc = e.mnc
			  AND x.category = e.category AND x.valid_from <= now()))
		ORDER BY mcc, mnc, category, valid_from`, id)
	if err != nil {
		writeDBErr(w, err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var e entry
		var scheduled bool
		if err := rows.Scan(&e.MCC, &e.MNC, &e.Category, &e.Price, &e.ValidFrom, &scheduled); err != nil {
			writeDBErr(w, err)
			return
		}
		out = append(out, map[string]any{"mcc": e.MCC, "mnc": e.MNC, "category": e.Category,
			"price": e.Price.StringFixed(4), "valid_from": e.ValidFrom.UTC().Format(time.RFC3339), "scheduled": scheduled})
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) putEntries(w http.ResponseWriter, r *http.Request, act actor) {
	id, ok := a.card(w, r, act)
	if !ok {
		return
	}
	var entries []entry
	if !decode(w, r, &entries) {
		return
	}
	a.saveEntries(w, r, "rate_card_entries", "rate_card_id", "price", id, entries)
}

// importEntries takes CSV: mcc,mnc,category,price[,valid_from RFC 3339].
func (a *API) importEntries(w http.ResponseWriter, r *http.Request, act actor) {
	id, ok := a.card(w, r, act)
	if !ok {
		return
	}
	entries, err := parseCSV(r.Body, "price")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	a.saveEntries(w, r, "rate_card_entries", "rate_card_id", "price", id, entries)
}

// importVendorCosts takes CSV: mcc,mnc,category,cost[,valid_from]. Platform only.
func (a *API) importVendorCosts(w http.ResponseWriter, r *http.Request, act actor) {
	if act.Role != "platform" {
		writeErr(w, http.StatusForbidden, "only the platform manages vendor costs")
		return
	}
	vendor := r.PathValue("vendor")
	var exists bool
	if err := a.PG.QueryRow(r.Context(), `SELECT EXISTS (SELECT 1 FROM vendors WHERE id = $1)`, vendor).Scan(&exists); err != nil || !exists {
		writeErr(w, http.StatusNotFound, "no such vendor")
		return
	}
	entries, err := parseCSV(r.Body, "cost")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	a.saveEntries(w, r, "vendor_costs", "vendor_id", "cost", vendor, entries)
}

// saveEntries validates every row, then upserts them in one transaction.
// A row with a future valid_from is a scheduled price change.
func (a *API) saveEntries(w http.ResponseWriter, r *http.Request, table, keyCol, priceCol, key string, entries []entry) {
	if len(entries) == 0 || len(entries) > 50000 {
		writeErr(w, http.StatusBadRequest, "send 1 to 50,000 rows")
		return
	}
	now := time.Now().UTC()
	for i := range entries {
		if err := entries[i].normalise(now); err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("row %d: %v", i+1, err))
			return
		}
	}
	q := fmt.Sprintf(`INSERT INTO %s (%s, mcc, mnc, category, %s, valid_from) VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (%s, mcc, mnc, category, valid_from) DO UPDATE SET %s = EXCLUDED.%s`,
		table, keyCol, priceCol, keyCol, priceCol, priceCol)
	err := pgx.BeginFunc(r.Context(), a.PG, func(tx pgx.Tx) error {
		b := &pgx.Batch{}
		for _, e := range entries {
			b.Queue(q, key, e.MCC, e.MNC, e.Category, e.Price, e.ValidFrom)
		}
		return tx.SendBatch(r.Context(), b).Close()
	})
	if err != nil {
		writeDBErr(w, err)
		return
	}
	scheduled := 0
	for _, e := range entries {
		if e.ValidFrom.After(now) {
			scheduled++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"saved": len(entries), "scheduled": scheduled,
		"note": "prices in force change within 1 minute (edge cache)"})
}

func parseCSV(body io.Reader, valueCol string) ([]entry, error) {
	cr := csv.NewReader(io.LimitReader(body, 16<<20))
	cr.TrimLeadingSpace = true
	header, err := cr.Read()
	if err != nil {
		return nil, fmt.Errorf("csv: %w", err)
	}
	idx := map[string]int{}
	for i, h := range header {
		idx[strings.ToLower(strings.TrimSpace(h))] = i
	}
	for _, need := range []string{"mcc", valueCol} {
		if _, ok := idx[need]; !ok {
			return nil, fmt.Errorf("csv header must include %q (columns: mcc,mnc,category,%s,valid_from)", need, valueCol)
		}
	}
	get := func(rec []string, col string) string {
		if i, ok := idx[col]; ok && i < len(rec) {
			return rec[i]
		}
		return ""
	}
	var out []entry
	for line := 2; ; line++ {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("csv line %d: %w", line, err)
		}
		v, err := decimal.NewFromString(strings.TrimSpace(get(rec, valueCol)))
		if err != nil {
			return nil, fmt.Errorf("csv line %d: %s %q is not a number", line, valueCol, get(rec, valueCol))
		}
		e := entry{MCC: get(rec, "mcc"), MNC: get(rec, "mnc"), Category: get(rec, "category"), Price: v}
		if s := strings.TrimSpace(get(rec, "valid_from")); s != "" {
			if e.ValidFrom, err = time.Parse(time.RFC3339, s); err != nil {
				return nil, fmt.Errorf("csv line %d: valid_from %q must be RFC 3339", line, s)
			}
		}
		out = append(out, e)
	}
	return out, nil
}

// transfer moves money from the acting reseller's wallet to one of its
// sub-accounts.
func (a *API) transfer(w http.ResponseWriter, r *http.Request, act actor) {
	if act.Role != "reseller" {
		writeErr(w, http.StatusForbidden, "transfers are for resellers funding their sub-accounts (the platform uses top-ups)")
		return
	}
	to, ok := a.managed(w, r, act)
	if !ok {
		return
	}
	var req topUpReq
	if !decode(w, r, &req) {
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		writeErr(w, http.StatusBadRequest, "Idempotency-Key header is required")
		return
	}
	bal, err := a.Ledger.Transfer(r.Context(), ledger.Transfer{
		From: act.AccountID, To: to, Amount: req.Amount, Reference: req.Reference, IdempotencyKey: key, Actor: act.User,
	})
	switch {
	case errors.Is(err, wallet.ErrInsufficient):
		writeErr(w, http.StatusUnprocessableEntity, "your available balance does not cover this transfer")
	case errors.Is(err, ledger.ErrDuplicate):
		writeErr(w, http.StatusConflict, "transfer with this Idempotency-Key already booked")
	case err != nil:
		a.Log.Error("transfer", "from", act.AccountID, "to", to, "err", err)
		writeErr(w, http.StatusBadRequest, err.Error())
	default:
		writeJSON(w, http.StatusCreated, map[string]string{"recipient_ledger_balance": bal.StringFixed(4)})
	}
}

// period reads ?from=&to= (RFC 3339 or YYYY-MM-DD); defaults to the current month.
func period(r *http.Request) (time.Time, time.Time, error) {
	now := time.Now().UTC()
	from := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	to := now
	parse := func(s string) (time.Time, error) {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t, nil
		}
		return time.Parse("2006-01-02", s)
	}
	var err error
	if s := r.URL.Query().Get("from"); s != "" {
		if from, err = parse(s); err != nil {
			return from, to, fmt.Errorf("from: use RFC 3339 or YYYY-MM-DD")
		}
	}
	if s := r.URL.Query().Get("to"); s != "" {
		if to, err = parse(s); err != nil {
			return from, to, fmt.Errorf("to: use RFC 3339 or YYYY-MM-DD")
		}
	}
	if !to.After(from) {
		return from, to, fmt.Errorf("to must be after from")
	}
	return from, to, nil
}

// statement lists ledger entries in a period with opening and closing balances.
func (a *API) statement(w http.ResponseWriter, r *http.Request, act actor) {
	id, ok := a.managedOrSelf(w, r, act)
	if !ok {
		return
	}
	from, to, err := period(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var opening decimal.Decimal
	err = a.PG.QueryRow(r.Context(), `SELECT coalesce((SELECT balance_after FROM ledger_entries
		WHERE account_id = $1 AND created_at < $2 ORDER BY created_at DESC, id DESC LIMIT 1), 0)`, id, from).Scan(&opening)
	if err != nil {
		writeDBErr(w, err)
		return
	}
	rows, err := a.PG.Query(r.Context(), `SELECT created_at, kind, amount, balance_after, coalesce(reference, ''),
		coalesce(quantity, 0), coalesce(counterparty::text, '')
		FROM ledger_entries WHERE account_id = $1 AND created_at >= $2 AND created_at < $3
		ORDER BY created_at, id LIMIT 100001`, id, from, to)
	if err != nil {
		writeDBErr(w, err)
		return
	}
	defer rows.Close()
	type line struct {
		At           string `json:"at"`
		Kind         string `json:"kind"`
		Amount       string `json:"amount"`
		Balance      string `json:"balance_after"`
		Reference    string `json:"reference,omitempty"`
		Messages     int    `json:"messages,omitempty"`
		Counterparty string `json:"counterparty,omitempty"`
	}
	var lines []line
	totals := map[string]decimal.Decimal{}
	closing := opening
	for rows.Next() {
		var at time.Time
		var kind, ref, cp string
		var amt, bal decimal.Decimal
		var qty int
		if err := rows.Scan(&at, &kind, &amt, &bal, &ref, &qty, &cp); err != nil {
			writeDBErr(w, err)
			return
		}
		lines = append(lines, line{At: at.UTC().Format(time.RFC3339), Kind: kind, Amount: amt.StringFixed(4),
			Balance: bal.StringFixed(4), Reference: ref, Messages: qty, Counterparty: cp})
		totals[kind] = totals[kind].Add(amt)
		closing = bal
	}
	if len(lines) > 100000 {
		writeErr(w, http.StatusRequestEntityTooLarge, "more than 100,000 entries; use a shorter period")
		return
	}
	t := map[string]string{}
	for k, v := range totals {
		t[k] = v.StringFixed(4)
	}
	if r.URL.Query().Get("format") == "csv" {
		w.Header().Set("Content-Type", "text/csv")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="statement-%s-%s.csv"`, id[:8], from.Format("2006-01")))
		cw := csv.NewWriter(w)
		_ = cw.Write([]string{"at", "kind", "amount", "balance_after", "reference", "messages", "counterparty"})
		for _, l := range lines {
			_ = cw.Write([]string{l.At, l.Kind, l.Amount, l.Balance, l.Reference, fmt.Sprint(l.Messages), l.Counterparty})
		}
		cw.Flush()
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"account_id": id, "from": from.Format(time.RFC3339), "to": to.Format(time.RFC3339),
		"opening_balance": opening.StringFixed(4), "closing_balance": closing.StringFixed(4),
		"totals": t, "entries": lines,
	})
}

// usage summarises messages per day from ClickHouse: count, segments,
// outcomes and the amount this account was charged (as sender or reseller).
func (a *API) usage(w http.ResponseWriter, r *http.Request, act actor) {
	id, ok := a.managedOrSelf(w, r, act)
	if !ok {
		return
	}
	if a.CH == nil {
		writeErr(w, http.StatusServiceUnavailable, "usage reports need CLICKHOUSE_URL")
		return
	}
	from, to, err := period(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// id is a validated UUID (managedOrSelf checked it against the database).
	rows, err := a.CH.Query(r.Context(), fmt.Sprintf(`
		SELECT toString(toDate(accepted_at)) AS day,
		       countIf(account_id = toUUID('%[1]s')) AS sent,
		       sumIf(segments, account_id = toUUID('%[1]s')) AS segments,
		       countIf(account_id = toUUID('%[1]s') AND status = 'delivered') AS delivered,
		       countIf(account_id = toUUID('%[1]s') AND status IN ('failed','expired','rejected')) AS not_delivered,
		       countIf(mapContains(charges, '%[1]s') AND account_id != toUUID('%[1]s')) AS via_sub_accounts,
		       toString(sum(charges['%[1]s'])) AS charged
		FROM message_records FINAL
		WHERE accepted_at >= %[2]s AND accepted_at < %[3]s AND mapContains(charges, '%[1]s')
		GROUP BY day ORDER BY day`, id, chTime(from), chTime(to)))
	if err != nil {
		a.Log.Error("usage query", "err", err)
		writeErr(w, http.StatusServiceUnavailable, "usage store unavailable")
		return
	}
	out := []map[string]string{}
	for _, row := range rows {
		if len(row) < 7 {
			continue
		}
		out = append(out, map[string]string{"day": row[0], "sent": row[1], "segments": row[2], "delivered": row[3],
			"not_delivered": row[4], "via_sub_accounts": row[5], "charged": row[6]})
	}
	writeJSON(w, http.StatusOK, map[string]any{"account_id": id, "from": from.Format(time.RFC3339), "to": to.Format(time.RFC3339), "days": out})
}
