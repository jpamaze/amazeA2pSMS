package adminapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Permission levels, lowest first.
type Permission int

const (
	PermRead    Permission = iota // viewer, support
	PermFinance                   // finance: top-ups, credit limits
	PermAdmin                     // admin, owner: everything
)

var permName = map[Permission]string{PermRead: "read", PermFinance: "finance", PermAdmin: "admin"}

// rolePerm maps users.role to the highest permission it holds.
var rolePerm = map[string]Permission{
	"viewer": PermRead, "support": PermRead, "finance": PermFinance, "admin": PermAdmin, "owner": PermAdmin,
}

var (
	errUnauthenticated = errors.New("missing or invalid credentials")
	errNoUser          = errors.New("signed in, but no platform user for this email")
)

// Resolver turns a request into the acting user.
type Resolver interface {
	Resolve(r *http.Request) (actor, error)
}

// DevHeaders trusts X-Actor-Account / X-Actor-User. Development only: it is
// used only when APP_ENV=dev and no OIDC issuer is configured. The actor
// gets admin permission on its account.
type DevHeaders struct {
	PG *pgxpool.Pool
}

// Resolve implements Resolver.
func (d DevHeaders) Resolve(r *http.Request) (actor, error) {
	act := actor{AccountID: r.Header.Get("X-Actor-Account"), User: r.Header.Get("X-Actor-User"), UserRole: "owner", Perm: PermAdmin}
	if act.AccountID == "" || act.User == "" {
		return act, errUnauthenticated
	}
	err := d.PG.QueryRow(r.Context(), `SELECT role FROM accounts WHERE id = $1 AND status = 'active'`, act.AccountID).Scan(&act.Role)
	if err != nil {
		return act, errUnauthenticated
	}
	return act, nil
}

// OIDC verifies Keycloak (or any OIDC provider) access tokens and maps the
// verified email to a row in users.
type OIDC struct {
	PG       *pgxpool.Pool
	Verifier *oidc.IDTokenVerifier
	Audience []string // client IDs accepted in aud or azp; empty skips the check
}

// NewOIDC discovers the issuer's keys. audience lists the client IDs
// (comma-separated: the admin tools and the portal) tokens must be issued for (Keycloak puts it in azp, or in aud with an audience
// mapper). discoveryURL, when set, is where to fetch discovery from if it
// differs from the issuer (e.g. Keycloak reached by its Docker service name).
func NewOIDC(ctx context.Context, pg *pgxpool.Pool, issuer, audience string, discoveryURL ...string) (*OIDC, error) {
	from := issuer
	if len(discoveryURL) > 0 && discoveryURL[0] != "" {
		from = discoveryURL[0]
		ctx = oidc.InsecureIssuerURLContext(ctx, issuer)
	}
	provider, err := oidc.NewProvider(ctx, from)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery for %s: %w", issuer, err)
	}
	return &OIDC{
		PG:       pg,
		Verifier: provider.Verifier(&oidc.Config{SkipClientIDCheck: true}),
		Audience: splitList(audience),
	}, nil
}

type tokenClaims struct {
	Email         string   `json:"email"`
	EmailVerified *bool    `json:"email_verified"`
	AuthorizedBy  string   `json:"azp"`
	Audience      audClaim `json:"aud"`
}

// audClaim accepts aud as a string or a list.
type audClaim []string

func (a *audClaim) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if strings.HasPrefix(s, "[") {
		var v []string
		if err := jsonUnmarshal(b, &v); err != nil {
			return err
		}
		*a = v
		return nil
	}
	var v string
	if err := jsonUnmarshal(b, &v); err != nil {
		return err
	}
	*a = []string{v}
	return nil
}

// Resolve implements Resolver.
func (o *OIDC) Resolve(r *http.Request) (actor, error) {
	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || raw == "" {
		return actor{}, errUnauthenticated
	}
	tok, err := o.Verifier.Verify(r.Context(), raw) // signature, issuer, expiry
	if err != nil {
		return actor{}, errUnauthenticated
	}
	var c tokenClaims
	if err := tok.Claims(&c); err != nil {
		return actor{}, errUnauthenticated
	}
	if len(o.Audience) > 0 && !slices.ContainsFunc(o.Audience, func(a string) bool {
		return c.AuthorizedBy == a || slices.Contains(c.Audience, a)
	}) {
		return actor{}, errUnauthenticated
	}
	if c.Email == "" || (c.EmailVerified != nil && !*c.EmailVerified) {
		return actor{}, errNoUser
	}

	var act actor
	err = o.PG.QueryRow(r.Context(), `
		UPDATE users u SET last_login_at = now()
		FROM accounts a
		WHERE lower(u.email) = lower($1) AND u.disabled_at IS NULL AND a.id = u.account_id AND a.status = 'active'
		RETURNING u.account_id::text, u.email, u.role, a.role`, c.Email).
		Scan(&act.AccountID, &act.User, &act.UserRole, &act.Role)
	if errors.Is(err, pgx.ErrNoRows) {
		return actor{}, errNoUser
	}
	if err != nil {
		return actor{}, err
	}
	act.Perm = rolePerm[act.UserRole]
	return act, nil
}

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
