package adminapi

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jpamaze/amazeA2pSMS/internal/platform/pg"
	"github.com/jpamaze/amazeA2pSMS/migrations"
)

// fakeIssuer serves OIDC discovery and a JWKS, and signs tokens like Keycloak.
type fakeIssuer struct {
	srv *httptest.Server
	key *rsa.PrivateKey
}

func newIssuer(t *testing.T) *fakeIssuer {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIssuer{key: key}
	mux := http.NewServeMux()
	f.srv = httptest.NewServer(mux)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": f.srv.URL, "jwks_uri": f.srv.URL + "/certs",
			"authorization_endpoint": f.srv.URL + "/auth", "token_endpoint": f.srv.URL + "/token",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/certs", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
			{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"},
		}})
	})
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeIssuer) token(t *testing.T, key *rsa.PrivateKey, claims map[string]any) string {
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "k1"))
	if err != nil {
		t.Fatal(err)
	}
	base := map[string]any{
		"iss": f.srv.URL, "sub": "u1", "azp": "amaze-admin", "aud": "account",
		"exp": time.Now().Add(5 * time.Minute).Unix(), "iat": time.Now().Unix(), "email_verified": true,
	}
	for k, v := range claims {
		base[k] = v
	}
	s, err := jwt.Signed(signer).Claims(base).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func testDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("POSTGRES_TEST_URL")
	if url == "" {
		url = "postgres://sms:sms@localhost:5432/sms_test?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err == nil {
		err = pool.Ping(ctx)
	}
	if err != nil {
		t.Skipf("postgres not reachable: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Migrate(ctx, pool, migrations.Postgres, "postgres", slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	return pool
}

func TestOIDCSignInAndRoles(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	var platform string
	if err := pool.QueryRow(ctx, `INSERT INTO accounts (role, name) VALUES ('platform', 'Amaze') RETURNING id`).Scan(&platform); err != nil {
		t.Fatal(err)
	}
	for email, role := range map[string]string{"owner@amaze.in": "owner", "Viewer@Amaze.in": "viewer", "fin@amaze.in": "finance"} {
		if _, err := pool.Exec(ctx, `INSERT INTO users (account_id, email, role) VALUES ($1, $2, $3)`, platform, email, role); err != nil {
			t.Fatal(err)
		}
	}

	iss := newIssuer(t)
	res, err := NewOIDC(ctx, pool, iss.srv.URL, "amaze-admin")
	if err != nil {
		t.Fatal(err)
	}
	api := &API{PG: pool, Resolver: res, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	h := api.Handler()

	call := func(method, path, tok, body string) (int, string) {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if tok != "" {
			r.Header.Set("Authorization", "Bearer "+tok)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code, w.Body.String()
	}

	owner := iss.token(t, iss.key, map[string]any{"email": "owner@amaze.in"})
	if code, body := call("GET", "/v1/me", owner, ""); code != 200 || !strings.Contains(body, `"user_role":"owner"`) {
		t.Fatalf("owner /me: %d %s", code, body)
	}

	// Email match is case-insensitive.
	viewer := iss.token(t, iss.key, map[string]any{"email": "viewer@amaze.in"})
	if code, _ := call("GET", "/v1/rate-cards", viewer, ""); code != 200 {
		t.Fatalf("viewer read: %d", code)
	}
	if code, _ := call("POST", "/v1/accounts/"+platform+"/users", viewer, `{"email":"x@y.z","role":"viewer"}`); code != 403 {
		t.Fatalf("viewer write must be forbidden: %d", code)
	}

	// Finance may not grant users; owner may, and only an owner grants owner.
	fin := iss.token(t, iss.key, map[string]any{"email": "fin@amaze.in"})
	if code, _ := call("POST", "/v1/accounts/"+platform+"/users", fin, `{"email":"a@b.c","role":"viewer"}`); code != 403 {
		t.Fatalf("finance granting users: %d", code)
	}
	if code, body := call("POST", "/v1/accounts/"+platform+"/users", owner, `{"email":"New.Admin@amaze.in","role":"admin"}`); code != 201 {
		t.Fatalf("owner grants admin: %d %s", code, body)
	}
	admin := iss.token(t, iss.key, map[string]any{"email": "new.admin@amaze.in"})
	if code, _ := call("POST", "/v1/accounts/"+platform+"/users", admin, `{"email":"o2@amaze.in","role":"owner"}`); code != 403 {
		t.Fatalf("admin granting owner: %d", code)
	}

	bad := map[string]string{
		"no token":         "",
		"expired":          iss.token(t, iss.key, map[string]any{"email": "owner@amaze.in", "exp": time.Now().Add(-time.Minute).Unix()}),
		"other client":     iss.token(t, iss.key, map[string]any{"email": "owner@amaze.in", "azp": "someone-else"}),
		"other issuer":     iss.token(t, iss.key, map[string]any{"email": "owner@amaze.in", "iss": "https://evil.example"}),
		"forged signature": iss.token(t, mustKey(t), map[string]any{"email": "owner@amaze.in"}),
	}
	for name, tok := range bad {
		if code, _ := call("GET", "/v1/me", tok, ""); code != 401 {
			t.Errorf("%s: want 401, got %d", name, code)
		}
	}
	if code, _ := call("GET", "/v1/me", iss.token(t, iss.key, map[string]any{"email": "stranger@x.in"}), ""); code != 403 {
		t.Errorf("unknown user: want 403, got %d", code)
	}
	if code, _ := call("GET", "/v1/me", iss.token(t, iss.key, map[string]any{"email": "owner@amaze.in", "email_verified": false}), ""); code != 403 {
		t.Errorf("unverified email: want 403, got %d", code)
	}

	// Changes are audited, reads are not.
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM admin_audit WHERE actor_email = 'owner@amaze.in' AND method = 'POST'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("audit rows for owner: %d %v", n, err)
	}
}

func mustKey(t *testing.T) *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k
}
