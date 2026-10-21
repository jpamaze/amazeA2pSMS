// Package adminapi manages accounts, users, wallets, credentials and webhooks.
//
// Callers sign in with Keycloak (OIDC bearer tokens, see auth.go); their
// users.role sets what they may do. Every change is written to admin_audit.
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
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/shopspring/decimal"

	"github.com/jpamaze/amazeA2pSMS/internal/auth"
	"github.com/jpamaze/amazeA2pSMS/internal/billing/credit"
	"github.com/jpamaze/amazeA2pSMS/internal/billing/ledger"
	"github.com/jpamaze/amazeA2pSMS/internal/billing/wallet"
	"github.com/jpamaze/amazeA2pSMS/internal/platform/secrets"
	"github.com/jpamaze/amazeA2pSMS/internal/records"
)

// API is the admin HTTP API.
type API struct {
	PG       *pgxpool.Pool
	Wallets  *wallet.Store
	Credits  *credit.Service
	Ledger   *ledger.Service
	Secrets  *secrets.Box
	Log      *slog.Logger
	Resolver Resolver
	CH       *records.ClickHouse   // usage reports; optional
	Redis    redis.UniversalClient // live deal counters; optional
	Portal   PortalConfig          // browser sign-in settings
	PromURL  string                // Prometheus for live monitoring; optional
}

type actor struct {
	AccountID string
	User      string // email
	Role      string // account role: platform | reseller | customer
	UserRole  string // users.role: owner | admin | finance | support | viewer
	Perm      Permission
}

// Handler returns the routed handler.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /v1/me", a.authed(PermRead, a.me))
	mux.Handle("POST /v1/accounts", a.authed(PermAdmin, a.createAccount))
	mux.Handle("POST /v1/accounts/{id}/users", a.authed(PermAdmin, a.createUser))
	mux.Handle("GET /v1/accounts/{id}/wallet", a.authed(PermRead, a.getWallet))
	mux.Handle("POST /v1/accounts/{id}/topups", a.authed(PermFinance, a.topUp))
	mux.Handle("PUT /v1/accounts/{id}/credit-limit", a.authed(PermFinance, a.setCreditLimit))
	mux.Handle("POST /v1/accounts/{id}/smpp-credentials", a.authed(PermAdmin, a.createSMPPCredential))
	mux.Handle("POST /v1/accounts/{id}/api-keys", a.authed(PermAdmin, a.createAPIKey))
	mux.Handle("PUT /v1/accounts/{id}/webhook", a.authed(PermAdmin, a.setWebhook))
	mux.Handle("PUT /v1/accounts/{id}/rate-card", a.authed(PermAdmin, a.setRateCard))
	mux.Handle("GET /v1/rate-cards", a.authed(PermRead, a.listRateCards))
	a.billingRoutes(mux)
	a.commercialRoutes(mux)
	a.dltRoutes(mux)
	a.networkRoutes(mux)
	a.portalRoutes(mux)
	a.portalDataRoutes(mux)
	a.opsRoutes(mux)
	return mux
}

// authed resolves the caller, checks its permission and audits changes.
func (a *API) authed(need Permission, h func(http.ResponseWriter, *http.Request, actor)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.Resolver == nil {
			writeErr(w, http.StatusServiceUnavailable, "no sign-in configured (set OIDC_ISSUER)")
			return
		}
		act, err := a.Resolver.Resolve(r)
		switch {
		case errors.Is(err, errNoUser):
			writeErr(w, http.StatusForbidden, err.Error())
			return
		case err != nil:
			w.Header().Set("WWW-Authenticate", `Bearer realm="amaze-admin"`)
			writeErr(w, http.StatusUnauthorized, errUnauthenticated.Error())
			return
		}
		if act.Perm < need {
			writeErr(w, http.StatusForbidden, "your role ("+act.UserRole+") does not allow this")
			return
		}
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		h(rec, r, act)
		if r.Method != http.MethodGet {
			a.audit(r, act, rec.status)
		}
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (a *API) audit(r *http.Request, act actor, status int) {
	ip, _, _ := strings.Cut(r.RemoteAddr, ":")
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		ip = strings.TrimSpace(strings.Split(fwd, ",")[0])
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := a.PG.Exec(ctx, `INSERT INTO admin_audit (actor_email, actor_acct, method, path, status, remote_ip)
		VALUES ($1, $2, $3, $4, $5, $6)`, act.User, act.AccountID, r.Method, r.URL.Path, status, ip); err != nil {
		a.Log.Error("admin audit write failed", "err", err, "actor", act.User, "path", r.URL.Path)
	}
}

func (a *API) me(w http.ResponseWriter, r *http.Request, act actor) {
	var name string
	_ = a.PG.QueryRow(r.Context(), `SELECT name FROM accounts WHERE id = $1`, act.AccountID).Scan(&name)
	writeJSON(w, http.StatusOK, map[string]any{
		"email": act.User, "account_id": act.AccountID, "account_name": name, "account_role": act.Role,
		"user_role": act.UserRole, "permission": permName[act.Perm], "brand": a.brandForAccount(r.Context(), act.AccountID),
	})
}

// createUser grants a person sign-in access to an account. The person signs
// in through Keycloak with this email; an owner role can only be granted by
// an owner.
func (a *API) createUser(w http.ResponseWriter, r *http.Request, act actor) {
	id, ok := a.managedOrSelf(w, r, act)
	if !ok {
		return
	}
	var req struct {
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if !decode(w, r, &req) {
		return
	}
	req.Email = strings.TrimSpace(strings.ToLower(req.Email))
	if _, known := rolePerm[req.Role]; !known || !strings.Contains(req.Email, "@") {
		writeErr(w, http.StatusBadRequest, "email and role (owner, admin, finance, support, viewer) are required")
		return
	}
	if req.Role == "owner" && act.UserRole != "owner" {
		writeErr(w, http.StatusForbidden, "only an owner can grant the owner role")
		return
	}
	var uid string
	if err := a.PG.QueryRow(r.Context(), `INSERT INTO users (account_id, email, role) VALUES ($1, $2, $3) RETURNING id`,
		id, req.Email, req.Role).Scan(&uid); err != nil {
		writeDBErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": uid, "email": req.Email, "role": req.Role})
}

// managedOrSelf allows the actor's own account as well as accounts it manages.
func (a *API) managedOrSelf(w http.ResponseWriter, r *http.Request, act actor) (string, bool) {
	if r.PathValue("id") == act.AccountID {
		return act.AccountID, true
	}
	return a.managed(w, r, act)
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
	if req.RateCardID != "" && parent == act.AccountID {
		if owns, err := a.ownsCard(r.Context(), act, req.RateCardID); err != nil || !owns {
			writeErr(w, http.StatusBadRequest, "rate card not found among yours")
			return
		}
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
	id, ok := a.managedOrSelf(w, r, act)
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
	id, ok := a.managedOrSelf(w, r, act)
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
	id, ok := a.managedOrSelf(w, r, act)
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
	if owns, err := a.ownsCard(r.Context(), act, req.RateCardID); err != nil || !owns {
		writeErr(w, http.StatusBadRequest, "rate card not found among yours")
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
	rows, err := a.PG.Query(r.Context(), `SELECT id, name, currency FROM rate_cards
		WHERE owner_account_id IS NOT DISTINCT FROM $1 ORDER BY name`, cardOwner(act))
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
