// Package adminapi manages accounts, wallets, credentials and webhooks.
//
// Authentication is a stand-in until Keycloak OIDC: the actor comes from the
// X-Actor-Account and X-Actor-User headers, so the API refuses to serve
// unless APP_ENV=dev.
package adminapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/jpamaze/amazeA2pSMS/internal/auth"
	"github.com/jpamaze/amazeA2pSMS/internal/billing/credit"
	"github.com/jpamaze/amazeA2pSMS/internal/billing/ledger"
	"github.com/jpamaze/amazeA2pSMS/internal/billing/wallet"
	"github.com/jpamaze/amazeA2pSMS/internal/platform/secrets"
)

// API is the admin HTTP API.
type API struct {
	PG      *pgxpool.Pool
	Wallets *wallet.Store
	Credits *credit.Service
	Ledger  *ledger.Service
	Secrets *secrets.Box
	Log     *slog.Logger
	DevAuth bool // header-based actor; only with APP_ENV=dev
}

type actor struct {
	AccountID string
	User      string
	Role      string
}

// Handler returns the routed handler.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST /v1/accounts", a.authed(a.createAccount))
	mux.Handle("GET /v1/accounts/{id}/wallet", a.authed(a.getWallet))
	mux.Handle("POST /v1/accounts/{id}/topups", a.authed(a.topUp))
	mux.Handle("PUT /v1/accounts/{id}/credit-limit", a.authed(a.setCreditLimit))
	mux.Handle("POST /v1/accounts/{id}/smpp-credentials", a.authed(a.createSMPPCredential))
	mux.Handle("POST /v1/accounts/{id}/api-keys", a.authed(a.createAPIKey))
	mux.Handle("PUT /v1/accounts/{id}/webhook", a.authed(a.setWebhook))
	mux.Handle("PUT /v1/accounts/{id}/rate-card", a.authed(a.setRateCard))
	mux.Handle("GET /v1/rate-cards", a.authed(a.listRateCards))
	return mux
}

func (a *API) authed(h func(http.ResponseWriter, *http.Request, actor)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.DevAuth {
			writeErr(w, http.StatusNotImplemented, "authentication not wired yet")
			return
		}
		act := actor{AccountID: r.Header.Get("X-Actor-Account"), User: r.Header.Get("X-Actor-User")}
		if act.AccountID == "" || act.User == "" {
			writeErr(w, http.StatusUnauthorized, "X-Actor-Account and X-Actor-User are required")
			return
		}
		err := a.PG.QueryRow(r.Context(), `SELECT role FROM accounts WHERE id = $1 AND status = 'active'`, act.AccountID).Scan(&act.Role)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "unknown actor account")
			return
		}
		h(w, r, act)
	})
}

// canManage: the platform manages every account; a reseller manages its
// direct sub-accounts.
func (a *API) canManage(ctx context.Context, act actor, accountID string) (bool, error) {
	if act.Role == "platform" {
		var ok bool
		err := a.PG.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM accounts WHERE id = $1)`, accountID).Scan(&ok)
		return ok, err
	}
	if act.Role != "reseller" {
		return false, nil
	}
	var parent *string
	err := a.PG.QueryRow(ctx, `SELECT parent_id::text FROM accounts WHERE id = $1`, accountID).Scan(&parent)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil && parent != nil && *parent == act.AccountID, err
}

func (a *API) managed(w http.ResponseWriter, r *http.Request, act actor) (string, bool) {
	id := r.PathValue("id")
	ok, err := a.canManage(r.Context(), act, id)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid account id")
		return "", false
	}
	if !ok {
		writeErr(w, http.StatusForbidden, "not allowed for this account")
		return "", false
	}
	return id, true
}

type createAccountReq struct {
	Name       string `json:"name"`
	Role       string `json:"role"`      // customer | reseller
	ParentID   string `json:"parent_id"` // platform only; defaults to the actor
	RateCardID string `json:"rate_card_id"`
}

func (a *API) createAccount(w http.ResponseWriter, r *http.Request, act actor) {
	var req createAccountReq
	if !decode(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Name) == "" || (req.Role != "customer" && req.Role != "reseller") {
		writeErr(w, http.StatusBadRequest, "name and role (customer or reseller) are required")
		return
	}
	parent := act.AccountID
	if req.ParentID != "" && req.ParentID != act.AccountID {
		if act.Role != "platform" {
			writeErr(w, http.StatusForbidden, "resellers create accounts under themselves only")
			return
		}
		parent = req.ParentID
	}
	if act.Role == "customer" {
		writeErr(w, http.StatusForbidden, "customers cannot create accounts")
		return
	}
	var id string
	var rateCard *string
	// The rate card defaults to the parent's; an account cannot send without one.
	err := a.PG.QueryRow(r.Context(), `
		INSERT INTO accounts (parent_id, role, name, rate_card_id)
		SELECT $1, $2, $3, coalesce(nullif($4, '')::uuid, p.rate_card_id) FROM accounts p WHERE p.id = $1
		RETURNING id, rate_card_id::text`, parent, req.Role, req.Name, req.RateCardID).Scan(&id, &rateCard)
	if err != nil {
		writeDBErr(w, err)
		return
	}
	if rateCard == nil && req.Role == "customer" {
		// Roll back: a customer without prices would have every message refused.
		_, _ = a.PG.Exec(r.Context(), `DELETE FROM wallets WHERE account_id = $1`, id)
		_, _ = a.PG.Exec(r.Context(), `DELETE FROM accounts WHERE id = $1`, id)
		writeErr(w, http.StatusBadRequest, "rate_card_id is required: the parent account has no rate card to inherit (GET /v1/rate-cards)")
		return
	}
	a.Log.Info("account created", "id", id, "parent", parent, "by", act.User)
	writeJSON(w, http.StatusCreated, map[string]string{"id": id})
}

func (a *API) getWallet(w http.ResponseWriter, r *http.Request, act actor) {
	id, ok := a.managed(w, r, act)
	if !ok {
		return
	}
	var bal, limit decimal.Decimal
	if err := a.PG.QueryRow(r.Context(), `SELECT balance, credit_limit FROM wallets WHERE account_id = $1`, id).Scan(&bal, &limit); err != nil {
		writeDBErr(w, err)
		return
	}
	snap, err := a.Wallets.Get(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "wallet cache unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"ledger_balance": bal.StringFixed(4), // settled in PostgreSQL
		"live_balance":   snap.Balance.Decimal().StringFixed(4),
		"credit_limit":   limit.StringFixed(4),
		"held":           snap.Held.Decimal().StringFixed(4),
		"available":      snap.Available().Decimal().StringFixed(4),
	})
}

type topUpReq struct {
	Amount    decimal.Decimal `json:"amount"`
	Reference string          `json:"reference"`
}

// topUp records money received. Only the platform books top-ups; reseller
// transfers to sub-accounts come later.
func (a *API) topUp(w http.ResponseWriter, r *http.Request, act actor) {
	if act.Role != "platform" {
		writeErr(w, http.StatusForbidden, "only the platform books top-ups")
		return
	}
	id, ok := a.managed(w, r, act)
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
	bal, err := a.Ledger.TopUp(r.Context(), ledger.TopUp{
		AccountID: id, Amount: req.Amount, Reference: req.Reference, IdempotencyKey: key, Actor: act.User,
	})
	switch {
	case errors.Is(err, ledger.ErrDuplicate):
		writeErr(w, http.StatusConflict, "top-up with this Idempotency-Key already booked")
	case err != nil:
		a.Log.Error("top-up", "account", id, "err", err)
		writeErr(w, http.StatusBadRequest, err.Error())
	default:
		writeJSON(w, http.StatusCreated, map[string]string{"ledger_balance": bal.StringFixed(4)})
	}
}

type creditLimitReq struct {
	CreditLimit decimal.Decimal `json:"credit_limit"`
	Reason      string          `json:"reason"`
}

func (a *API) setCreditLimit(w http.ResponseWriter, r *http.Request, act actor) {
	var req creditLimitReq
	if !decode(w, r, &req) {
		return
	}
	err := a.Credits.SetLimit(r.Context(), credit.Change{
		AccountID: r.PathValue("id"), NewLimit: req.CreditLimit,
		ActorAccountID: act.AccountID, ActorUser: act.User, Reason: req.Reason,
	})
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, credit.ErrNoAccount):
		writeErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, credit.ErrNotAllowed):
		writeErr(w, http.StatusForbidden, err.Error())
	case errors.Is(err, credit.ErrResellerLimit), errors.Is(err, wallet.ErrNegativeLimit):
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
	default:
		writeErr(w, http.StatusBadRequest, err.Error())
	}
}

type smppCredReq struct {
	SystemID   string   `json:"system_id"`
	Password   string   `json:"password"`
	TPS        int      `json:"tps"`
	MaxBinds   int      `json:"max_binds"`
	AllowedIPs []string `json:"allowed_ips"`
}

func (a *API) createSMPPCredential(w http.ResponseWriter, r *http.Request, act actor) {
	id, ok := a.managed(w, r, act)
	if !ok {
		return
	}
	var req smppCredReq
	if !decode(w, r, &req) {
		return
	}
	if req.SystemID == "" || len(req.SystemID) > 15 || len(req.Password) < 6 || len(req.Password) > 8 {
		// SMPP limits: system_id up to 15 characters, password up to 8.
		writeErr(w, http.StatusBadRequest, "system_id (1-15 chars) and password (6-8 chars) are required")
		return
	}
	var prefixes []netip.Prefix
	for _, s := range req.AllowedIPs {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			addr, aerr := netip.ParseAddr(s)
			if aerr != nil {
				writeErr(w, http.StatusBadRequest, "allowed_ips: "+s+" is not an IP or CIDR")
				return
			}
			p = netip.PrefixFrom(addr, addr.BitLen())
		}
		prefixes = append(prefixes, p)
	}
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "hash failed")
		return
	}
	var credID string
	err = a.PG.QueryRow(r.Context(), `INSERT INTO smpp_credentials
		(account_id, system_id, password_hash, allowed_ips, max_binds, tps_limit)
		VALUES ($1, $2, $3, $4, coalesce(nullif($5, 0), 4), coalesce(nullif($6, 0), 100)) RETURNING id`,
		id, req.SystemID, hash, prefixes, req.MaxBinds, req.TPS).Scan(&credID)
	if err != nil {
		writeDBErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": credID, "system_id": req.SystemID})
}

func (a *API) createAPIKey(w http.ResponseWriter, r *http.Request, act actor) {
	id, ok := a.managed(w, r, act)
	if !ok {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Name == "" {
		req.Name = "default"
	}
	full, prefix, hash, err := auth.NewAPIKey()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "key generation failed")
		return
	}
	if _, err := a.PG.Exec(r.Context(), `INSERT INTO api_keys (account_id, prefix, key_hash, name) VALUES ($1, $2, $3, $4)`,
		id, prefix, hash, req.Name); err != nil {
		writeDBErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"api_key": full, "note": "shown once; store it now"})
}

// setWebhook sets the account's delivery-status webhook and returns a new
// signing secret (shown once).
func (a *API) setWebhook(w http.ResponseWriter, r *http.Request, act actor) {
	id, ok := a.managed(w, r, act)
	if !ok {
		return
	}
	var req struct {
		URL string `json:"url"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.URL == "" {
		if _, err := a.PG.Exec(r.Context(), `UPDATE accounts SET webhook_url = NULL, webhook_secret_enc = NULL WHERE id = $1`, id); err != nil {
			writeDBErr(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	u, err := url.Parse(req.URL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		writeErr(w, http.StatusBadRequest, "url must be an absolute http(s) URL")
		return
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		writeErr(w, http.StatusInternalServerError, "secret generation failed")
		return
	}
	secret := "whsec_" + hex.EncodeToString(raw)
	enc, err := a.Secrets.Seal([]byte(secret))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "secret encryption failed")
		return
	}
	if _, err := a.PG.Exec(r.Context(), `UPDATE accounts SET webhook_url = $2, webhook_secret_enc = $3 WHERE id = $1`,
		id, req.URL, enc); err != nil {
		writeDBErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"url": req.URL, "secret": secret, "note": "shown once; verify X-Amaze-Signature with it"})
}

func (a *API) setRateCard(w http.ResponseWriter, r *http.Request, act actor) {
	id, ok := a.managed(w, r, act)
	if !ok {
		return
	}
	var req struct {
		RateCardID string `json:"rate_card_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	tag, err := a.PG.Exec(r.Context(), `UPDATE accounts SET rate_card_id = $2 WHERE id = $1`, id, req.RateCardID)
	if err != nil {
		writeDBErr(w, err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	// Pricing caches each answer for up to a minute on every edge node.
	writeJSON(w, http.StatusOK, map[string]string{"rate_card_id": req.RateCardID, "note": "takes effect within 1 minute"})
}

func (a *API) listRateCards(w http.ResponseWriter, r *http.Request, act actor) {
	if act.Role == "customer" {
		writeErr(w, http.StatusForbidden, "not allowed")
		return
	}
	rows, err := a.PG.Query(r.Context(), `SELECT id, name, currency FROM rate_cards ORDER BY name`)
	if err != nil {
		writeDBErr(w, err)
		return
	}
	defer rows.Close()
	out := []map[string]string{}
	for rows.Next() {
		var id, name, cur string
		if err := rows.Scan(&id, &name, &cur); err != nil {
			writeDBErr(w, err)
			return
		}
		out = append(out, map[string]string{"id": id, "name": name, "currency": cur})
	}
	writeJSON(w, http.StatusOK, out)
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return false
	}
	return true
}

func writeDBErr(w http.ResponseWriter, err error) {
	var pgErr *pgconn.PgError
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		writeErr(w, http.StatusNotFound, "not found")
	case errors.As(err, &pgErr) && pgErr.Code == "23505":
		writeErr(w, http.StatusConflict, "already exists")
	case errors.As(err, &pgErr) && (pgErr.Code == "23514" || pgErr.Code == "22P02" || pgErr.Code == "23503"):
		writeErr(w, http.StatusBadRequest, pgErr.Message)
	default:
		writeErr(w, http.StatusInternalServerError, "database error")
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
