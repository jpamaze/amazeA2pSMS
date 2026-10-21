# Admin sign-in

The admin API (`cmd/admin-api`) accepts OIDC bearer tokens from Keycloak. Passwords, 2FA and lockout live in Keycloak. The platform keeps only who may do what: the `users` table maps a verified email to an account and a role.

| users.role | May |
| --- | --- |
| viewer, support | read (wallets, rate cards, `GET /v1/me`) |
| finance | read, top-ups, credit limits |
| admin | everything, except granting the owner role |
| owner | everything |

A platform user acts on any account. A reseller's user acts on the reseller's own account and its direct sub-accounts. Every change (any non-GET request) is written to `admin_audit` with the email, account, path, result and source IP.

## Configuration

| Variable | Meaning |
| --- | --- |
| `OIDC_ISSUER` | Keycloak realm URL, e.g. `https://sso.example.com/realms/amaze`. Required outside `APP_ENV=dev`. |
| `OIDC_AUDIENCE` | Client the tokens must be issued for (`azp` or `aud`); default `amaze-admin`. |
| `OIDC_DISCOVERY_URL` | Optional. Where to fetch discovery when it differs from the issuer (for example the Docker service name). |

Tokens are checked for signature (realm keys, refreshed automatically), issuer, expiry and audience. The email must be verified and match an enabled user on an active account. A wrong or expired token gets 401; a valid token for someone with no platform user gets 403.

With `APP_ENV=dev` and no `OIDC_ISSUER`, the API falls back to `X-Actor-Account` / `X-Actor-User` headers (acting as owner) so local scripts keep working. That fallback never runs outside dev.

## Production setup

1. Create a realm `amaze` in Keycloak. Import `deploy/keycloak/amaze-realm.json` as a starting point, then delete its demo user, turn off `directAccessGrantsEnabled`, set real redirect URIs, and require OTP for the realm.
2. Create the people in Keycloak, with verified emails.
3. Grant the first platform owner with the `admin-user` tool (from any machine that can reach PostgreSQL, with `POSTGRES_URL` set): `admin-user -email owner@yourcompany.com -role owner`.
4. After that, owners and admins grant access through the API: `POST /v1/accounts/{id}/users {"email":"…","role":"finance"}`.

## Dev (Docker Compose)

`make up` starts Keycloak on `localhost:8180` with the realm imported, and the seed grants `ops@amaze.local` owner on the platform account:

```sh
TOKEN=$(curl -s -d grant_type=password -d client_id=amaze-admin \
  -d username=ops@amaze.local -d password=dev-only-change-me \
  http://localhost:8180/realms/amaze/protocol/openid-connect/token | jq -r .access_token)
curl -s localhost:8081/v1/me -H "Authorization: Bearer $TOKEN"
```

## Portal sign-in

The portal (phase 4) signs in with the public client `amaze-portal` (authorization code with PKCE); admin-api accepts tokens issued to `amaze-admin` or `amaze-portal` (`OIDC_AUDIENCE`).
- Add every portal origin to the client: `https://<domain>/callback/` as a redirect URI and `https://<domain>/login/` as a post-logout redirect URI, reseller white-label domains included.
- Wildcards are not used, so a code cannot be sent to another site.
- `PORTAL_OIDC_ISSUER` sets the realm URL browsers use, when it differs from `OIDC_ISSUER`.

