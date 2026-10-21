// Package httpapi is the customer REST edge (api/openapi.yaml).
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jpamaze/amazeA2pSMS/internal/platform/metrics"

	"golang.org/x/time/rate"

	"github.com/jpamaze/amazeA2pSMS/internal/auth"
	"github.com/jpamaze/amazeA2pSMS/internal/billing/wallet"
	"github.com/jpamaze/amazeA2pSMS/internal/dlt"
	"github.com/jpamaze/amazeA2pSMS/internal/domain"
	"github.com/jpamaze/amazeA2pSMS/internal/firewall"
	"github.com/jpamaze/amazeA2pSMS/internal/ingest"
	"github.com/jpamaze/amazeA2pSMS/internal/msgstore"
)

// KeyChecker is the subset of auth.Authenticator the API needs.
type KeyChecker interface {
	APIKey(ctx context.Context, key string) (string, error)
}

// Acceptor is the subset of ingest.Pipeline the API needs.
type Acceptor interface {
	Accept(ctx context.Context, req ingest.Request) (ingest.Result, error)
}

// API serves the customer endpoints.
type API struct {
	Keys       KeyChecker
	Accept     Acceptor
	Store      *msgstore.Store
	Wallets    *wallet.Store
	Log        *slog.Logger
	AccountTPS int // per-account request limit on this node

	mu       sync.Mutex
	limiters map[string]*rate.Limiter
}

// Handler returns the routed handler.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST /v1/sms", a.authed(a.send))
	mux.Handle("GET /v1/sms/{id}", a.authed(a.status))
	mux.Handle("GET /v1/wallet", a.authed(a.wallet))
	return mux
}

func (a *API) authed(h func(http.ResponseWriter, *http.Request, string)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("X-API-Key")
		if key == "" {
			if v, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
				key = v
			}
		}
		acct, err := a.Keys.APIKey(r.Context(), key)
		if err != nil {
			if errors.Is(err, auth.ErrInvalidCredentials) || errors.Is(err, auth.ErrAccountInactive) {
				writeErr(w, http.StatusUnauthorized, "unauthorized", "invalid or missing API key")
				return
			}
			writeErr(w, http.StatusServiceUnavailable, "unavailable", "try again")
			return
		}
		h(w, r, acct)
	})
}

func (a *API) limiter(acct string) *rate.Limiter {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.limiters == nil {
		a.limiters = map[string]*rate.Limiter{}
	}
	l, ok := a.limiters[acct]
	if !ok {
		tps := max(a.AccountTPS, 1)
		l = rate.NewLimiter(rate.Limit(tps), max(1, tps/5))
		a.limiters[acct] = l
	}
	return l
}

type sendRequest struct {
	To         string `json:"to"`
	Sender     string `json:"sender"`
	Text       string `json:"text"`
	PEID       string `json:"pe_id"`
	TemplateID string `json:"template_id"`
	TMChain    string `json:"tm_chain"`
	ClientRef  string `json:"client_ref"`
}

type sendResponse struct {
	ID        string `json:"id"`
	Segments  int    `json:"segments"`
	Cost      string `json:"cost"`
	Status    string `json:"status"`
	Duplicate bool   `json:"duplicate,omitempty"`
}

func (a *API) send(w http.ResponseWriter, r *http.Request, acct string) {
	if !a.limiter(acct).Allow() {
		metrics.Refused.WithLabelValues("http", "throttled").Inc()
		writeErr(w, http.StatusTooManyRequests, "throttled", "request rate above account limit")
		return
	}
	var req sendRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if len(req.ClientRef) > 64 || len(r.Header.Get("Idempotency-Key")) > 64 {
		writeErr(w, http.StatusBadRequest, "invalid_request", "client_ref and Idempotency-Key are limited to 64 characters")
		return
	}
	res, err := a.Accept.Accept(r.Context(), ingest.Request{
		AccountID: acct, Channel: domain.ChannelHTTP, Sender: req.Sender, Destination: req.To, Text: req.Text,
		DLT:                domain.DLTFields{PEID: req.PEID, TemplateID: req.TemplateID, TMChain: req.TMChain},
		RegisteredDelivery: true, ClientRef: req.ClientRef, IdempotencyKey: r.Header.Get("Idempotency-Key"),
	})
	if err != nil {
		code, kind := statusFor(err)
		if code == http.StatusServiceUnavailable {
			a.Log.Warn("send unavailable", "account", acct, "err", err)
		}
		writeErr(w, code, kind, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, sendResponse{
		ID: res.MessageID, Segments: res.Segments, Cost: res.Price.StringFixed(4),
		Status: string(domain.StatusAccepted), Duplicate: res.Duplicate,
	})
}

func statusFor(err error) (int, string) {
	switch {
	case errors.Is(err, ingest.ErrInvalidDestination):
		return http.StatusBadRequest, "invalid_destination"
	case errors.Is(err, ingest.ErrInvalidSender):
		return http.StatusBadRequest, "invalid_sender"
	case errors.Is(err, ingest.ErrEmptyMessage), errors.Is(err, ingest.ErrTooLong):
		return http.StatusBadRequest, "invalid_text"
	case errors.Is(err, ingest.ErrInsufficient):
		return http.StatusPaymentRequired, "insufficient_balance"
	case errors.Is(err, ingest.ErrNoPrice):
		return http.StatusUnprocessableEntity, "no_rate"
	case errors.Is(err, ingest.ErrInactive):
		return http.StatusForbidden, "account_inactive"
	case errors.Is(err, ingest.ErrResellerBalance), errors.Is(err, ingest.ErrResellerNoPrice):
		return http.StatusPaymentRequired, "reseller_billing"
	}
	if e, ok := dlt.AsError(err); ok {
		return http.StatusUnprocessableEntity, string(e.Reason)
	}
	if e, ok := firewall.AsError(err); ok {
		return http.StatusUnprocessableEntity, string(e.Reason)
	}
	return http.StatusServiceUnavailable, "unavailable"
}

type statusResponse struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	ErrorCode   string `json:"error_code,omitempty"`
	To          string `json:"to"`
	Sender      string `json:"sender"`
	Segments    int    `json:"segments"`
	Cost        string `json:"cost"`
	ClientRef   string `json:"client_ref,omitempty"`
	AcceptedAt  string `json:"accepted_at"`
	CompletedAt string `json:"completed_at,omitempty"`
}

func (a *API) status(w http.ResponseWriter, r *http.Request, acct string) {
	st, err := a.Store.Get(r.Context(), r.PathValue("id"))
	if errors.Is(err, msgstore.ErrNotFound) || (err == nil && st.AccountID != acct) {
		writeErr(w, http.StatusNotFound, "not_found", "no such message")
		return
	}
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "unavailable", "try again")
		return
	}
	resp := statusResponse{
		ID: st.ID, Status: string(st.Status), ErrorCode: st.ErrorCode, To: st.Destination, Sender: st.Sender,
		Segments: st.Segments, Cost: st.Price, ClientRef: st.ClientRef, AcceptedAt: st.AcceptedAt.Format(time.RFC3339Nano),
	}
	if !st.DoneAt.IsZero() {
		resp.CompletedAt = st.DoneAt.Format(time.RFC3339Nano)
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *API) wallet(w http.ResponseWriter, r *http.Request, acct string) {
	s, err := a.Wallets.Get(r.Context(), acct)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "unavailable", "try again")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"currency":     "INR",
		"balance":      s.Balance.Decimal().StringFixed(4),
		"credit_limit": s.CreditLimit.Decimal().StringFixed(4),
		"held":         s.Held.Decimal().StringFixed(4),
		"available":    s.Available().Decimal().StringFixed(4),
		"credit_used":  s.CreditUsed().Decimal().StringFixed(4),
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, kind, msg string) {
	writeJSON(w, code, map[string]string{"code": kind, "error": msg})
}
