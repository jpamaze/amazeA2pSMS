package adminapi

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
)

// PortalConfig is what the browser needs before signing in.
type PortalConfig struct {
	OIDCIssuer   string // public realm URL; empty in dev (header sign-in)
	OIDCClientID string // public client with PKCE, e.g. amaze-portal
}

// Brand is how the portal looks for an account or a domain.
type Brand struct {
	AccountID    string `json:"account_id,omitempty"`
	Name         string `json:"name"`
	Logo         string `json:"logo"`
	PrimaryColor string `json:"primary_color"`
	SupportEmail string `json:"support_email"`
	Domain       string `json:"domain,omitempty"`
}

// DefaultBrand is used when neither the domain nor the platform has one.
var DefaultBrand = Brand{Name: "Amaze SMS", PrimaryColor: "#1f6feb"}

func (a *API) portalRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/portal/config", a.portalConfig)
	mux.HandleFunc("GET /v1/portal/dev-users", a.devUsers)
	mux.Handle("GET /v1/accounts/{id}/branding", a.authed(PermRead, a.getBranding))
	mux.Handle("PUT /v1/accounts/{id}/branding", a.authed(PermAdmin, a.putBranding))
}

func scanBrand(row pgx.Row) (Brand, error) {
	var b Brand
	var domain *string
	err := row.Scan(&b.AccountID, &b.Name, &b.Logo, &b.PrimaryColor, &b.SupportEmail, &domain)
	if domain != nil {
		b.Domain = *domain
	}
	return b, err
}

const brandCols = `account_id::text, display_name, logo, primary_color, support_email, domain`

// brandForHost is the brand of a portal domain, else the platform's.
func (a *API) brandForHost(ctx context.Context, host string) Brand {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	b, err := scanBrand(a.PG.QueryRow(ctx, `SELECT `+brandCols+` FROM branding WHERE domain = lower($1)`, host))
	if err == nil {
		return b
	}
	return a.platformBrand(ctx)
}

func (a *API) platformBrand(ctx context.Context) Brand {
	b, err := scanBrand(a.PG.QueryRow(ctx, `SELECT `+brandCols+` FROM branding b JOIN accounts x ON x.id = b.account_id
		WHERE x.role = 'platform' LIMIT 1`))
	if err != nil {
		return DefaultBrand
	}
	return b
}

// brandForAccount is the nearest brand up the account's chain: its own (a
// reseller), its reseller's, then the platform's.
func (a *API) brandForAccount(ctx context.Context, accountID string) Brand {
	b, err := scanBrand(a.PG.QueryRow(ctx, `WITH RECURSIVE up(id, parent_id, depth) AS (
			SELECT id, parent_id, 0 FROM accounts WHERE id = $1
			UNION ALL SELECT x.id, x.parent_id, up.depth + 1 FROM accounts x JOIN up ON x.id = up.parent_id)
		SELECT `+brandCols+` FROM branding JOIN up ON up.id = branding.account_id ORDER BY up.depth LIMIT 1`, accountID))
	if err != nil {
		return DefaultBrand
	}
	return b
}

func (a *API) portalConfig(w http.ResponseWriter, r *http.Request) {
	cfg := map[string]any{"brand": a.brandForHost(r.Context(), r.Host)}
	if a.Portal.OIDCIssuer != "" {
		cfg["oidc"] = map[string]string{"issuer": a.Portal.OIDCIssuer, "client_id": a.Portal.OIDCClientID}
	} else if _, dev := a.Resolver.(DevHeaders); dev {
		cfg["dev_sign_in"] = true
	}
	writeJSON(w, http.StatusOK, cfg)
}

// devUsers lists users to pick from on the dev sign-in page. It answers only
// when sign-in is by dev headers (APP_ENV=dev without OIDC).
func (a *API) devUsers(w http.ResponseWriter, r *http.Request) {
	if _, dev := a.Resolver.(DevHeaders); !dev {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	rows, err := a.PG.Query(r.Context(), `SELECT u.email, u.role, a.id, a.name, a.role FROM users u JOIN accounts a ON a.id = u.account_id
		WHERE u.disabled_at IS NULL ORDER BY a.role, a.name, u.email LIMIT 200`)
	if err != nil {
		writeDBErr(w, err)
		return
	}
	defer rows.Close()
	out := []map[string]string{}
	for rows.Next() {
		var email, urole, id, name, role string
		if err := rows.Scan(&email, &urole, &id, &name, &role); err != nil {
			writeDBErr(w, err)
			return
		}
		out = append(out, map[string]string{"email": email, "user_role": urole, "account_id": id, "account_name": name, "account_role": role})
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) getBranding(w http.ResponseWriter, r *http.Request, act actor) {
	id, ok := a.managedOrSelf(w, r, act)
	if !ok {
		return
	}
	b, err := scanBrand(a.PG.QueryRow(r.Context(), `SELECT `+brandCols+` FROM branding WHERE account_id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusOK, map[string]any{"own": nil, "effective": a.brandForAccount(r.Context(), id)})
		return
	}
	if err != nil {
		writeDBErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"own": b, "effective": b})
}

var (
	colorRe = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
	emailRe = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
)

// putBranding sets a reseller's (or the platform's) brand.
func (a *API) putBranding(w http.ResponseWriter, r *http.Request, act actor) {
	id, ok := a.managedOrSelf(w, r, act)
	if !ok {
		return
	}
	var role string
	if err := a.PG.QueryRow(r.Context(), `SELECT role FROM accounts WHERE id = $1`, id).Scan(&role); err != nil {
		writeDBErr(w, err)
		return
	}
	if role == "customer" {
		writeErr(w, http.StatusBadRequest, "only resellers and the platform have a brand")
		return
	}
	var req struct {
		Name         string `json:"name"`
		Logo         string `json:"logo"`
		PrimaryColor string `json:"primary_color"`
		SupportEmail string `json:"support_email"`
		Domain       string `json:"domain"`
	}
	if !decode(w, r, &req) {
		return
	}
	req.Domain = strings.ToLower(strings.TrimSpace(req.Domain))
	switch {
	case strings.TrimSpace(req.Name) == "" || len(req.Name) > 60 || hasControl(req.Name):
		writeErr(w, http.StatusBadRequest, "name is required (up to 60 characters, no line breaks)")
		return
	case req.PrimaryColor != "" && !colorRe.MatchString(req.PrimaryColor):
		writeErr(w, http.StatusBadRequest, "primary_color must look like #1f6feb")
		return
	case req.Logo != "" && !strings.HasPrefix(req.Logo, "https://") && !strings.HasPrefix(req.Logo, "data:image/"):
		writeErr(w, http.StatusBadRequest, "logo must be an https:// URL or a data:image/ URI")
		return
	case req.SupportEmail != "" && !emailRe.MatchString(req.SupportEmail):
		writeErr(w, http.StatusBadRequest, "support_email is not an e-mail address")
		return
	}
	if req.PrimaryColor == "" {
		req.PrimaryColor = DefaultBrand.PrimaryColor
	}
	// Portal domains are the platform's to set: it also adds the TLS
	// certificate and the sign-in redirect, and nobody else may claim a host
	// name (the platform's own portal included). A reseller saving its brand
	// keeps whatever domain it has.
	if act.Role != "platform" {
		var current string
		if err := a.PG.QueryRow(r.Context(), `SELECT coalesce(domain, '') FROM branding WHERE account_id = $1`, id).Scan(&current); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			writeDBErr(w, err)
			return
		}
		if req.Domain != current {
			writeErr(w, http.StatusForbidden, "the portal domain is set by the platform; ask it to add yours")
			return
		}
	}
	var domain *string
	if req.Domain != "" {
		domain = &req.Domain
	}
	_, err := a.PG.Exec(r.Context(), `INSERT INTO branding (account_id, domain, display_name, logo, primary_color, support_email, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (account_id) DO UPDATE SET domain = EXCLUDED.domain, display_name = EXCLUDED.display_name, logo = EXCLUDED.logo,
		  primary_color = EXCLUDED.primary_color, support_email = EXCLUDED.support_email, updated_by = EXCLUDED.updated_by, updated_at = now()`,
		id, domain, req.Name, req.Logo, req.PrimaryColor, req.SupportEmail, act.User)
	if err != nil {
		writeDBErr(w, err) // 409 when the domain is taken, 400 when it is malformed
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}

// PortalHandler serves the API under /v1/ and the portal's static export
// everywhere else, falling back to index.html for client-side routes.
func PortalHandler(dir string, api http.Handler) http.Handler {
	files := http.FileServer(http.Dir(dir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/") {
			api.ServeHTTP(w, r)
			return
		}
		p := path.Clean("/" + r.URL.Path)
		full := filepath.Join(dir, filepath.FromSlash(p))
		if st, err := os.Stat(full); err != nil || (st.IsDir() && !exists(filepath.Join(full, "index.html"))) {
			if exists(full + ".html") {
				r.URL.Path = p + ".html"
			} else {
				r.URL.Path = "/"
			}
		}
		if strings.HasPrefix(p, "/_next/static/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("X-Frame-Options", "DENY")
		files.ServeHTTP(w, r)
	})
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// SecureHeaders sets the security headers of every response: API answers
// are never cached (statements, keys shown once, secrets); portal pages get
// a Content-Security-Policy that keeps scripts, styles and API calls on this
// origin and the sign-in server (oidcIssuer, may be empty); HTTPS responses
// get HSTS.
func SecureHeaders(h http.Handler, oidcIssuer string) http.Handler {
	sso := ""
	if u, err := url.Parse(oidcIssuer); err == nil && u.Scheme != "" && u.Host != "" {
		sso = " " + u.Scheme + "://" + u.Host
	}
	// Next.js bootstraps pages with inline scripts, hence 'unsafe-inline';
	// connect-src still stops a script from sending data anywhere else.
	csp := "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data: https:; " +
		"connect-src 'self'" + sso + "; frame-src 'self'" + sso + "; form-action 'self'" + sso + "; " +
		"frame-ancestors 'none'; base-uri 'self'; object-src 'none'"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("X-Content-Type-Options", "nosniff")
		if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
			hd.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		if strings.HasPrefix(r.URL.Path, "/v1/") {
			hd.Set("Cache-Control", "no-store")
		} else {
			hd.Set("Content-Security-Policy", csp)
		}
		h.ServeHTTP(w, r)
	})
}
