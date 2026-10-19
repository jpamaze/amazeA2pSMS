// Command admin-api manages accounts, credentials, DLT registrations, vendors,
// routes and credit limits for the platform, resellers and customers.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/caarlos0/env/v11"
	"github.com/shopspring/decimal"

	"github.com/jpamaze/amazeA2pSMS/internal/billing/credit"
	"github.com/jpamaze/amazeA2pSMS/internal/billing/wallet"
	"github.com/jpamaze/amazeA2pSMS/internal/service"
)

type adminConfig struct {
	Addr string `env:"ADMIN_ADDR" envDefault:":8081"`
}

func main() {
	var ac adminConfig
	service.Run("admin-api", service.Needs{Postgres: true, Redis: true}, func(ctx context.Context, d *service.Deps) error {
		if err := env.Parse(&ac); err != nil {
			return fmt.Errorf("admin-api config: %w", err)
		}
		credits := credit.New(d.PG, wallet.New(d.Redis))

		mux := http.NewServeMux()
		mux.Handle("PUT /v1/accounts/{id}/credit-limit", setCreditLimit(credits, d.Cfg.Env))

		d.Log.Info("admin api listening", "addr", ac.Addr)
		return service.ServeHTTP(ctx, &http.Server{
			Addr:              ac.Addr,
			Handler:           mux,
			ReadHeaderTimeout: 5 * time.Second,
		})
	})
}

type creditLimitRequest struct {
	CreditLimit decimal.Decimal `json:"credit_limit"`
	Reason      string          `json:"reason"`
}

// setCreditLimit changes an account's credit limit. The platform may change any
// account; a reseller only its own sub-accounts.
//
// TODO(phase 0 -> 1): the actor comes from X-Actor-* headers until Keycloak
// OIDC is wired in, so the endpoint is enabled only when APP_ENV=dev.
func setCreditLimit(svc *credit.Service, appEnv string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if appEnv != "dev" {
			writeErr(w, http.StatusNotImplemented, "authentication not wired yet")
			return
		}
		var req creditLimitRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		err := svc.SetLimit(r.Context(), credit.Change{
			AccountID:      r.PathValue("id"),
			NewLimit:       req.CreditLimit,
			ActorAccountID: r.Header.Get("X-Actor-Account"),
			ActorUser:      r.Header.Get("X-Actor-User"),
			Reason:         req.Reason,
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
	})
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
