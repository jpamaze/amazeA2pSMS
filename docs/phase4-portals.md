# Phase 4 slice 1: portals, reports, live monitoring, alerts

One web app serves the admin (platform), reseller and customer portals; what each user sees follows their account's role and their own user role. Slice 2 adds the DLT and platform operations screens (vendors, routes, firewall, deals, disputes, rate cards), which today are API-only.

## How it is built

| Part | Choice |
| --- | --- |
| App | `portal/`: Next.js 15 + TypeScript, built as a static export (`npm run build` → `portal/out`), no Node server in production |
| Serving | admin-api serves `portal/out` at `/` (`PORTAL_DIR`) next to the API at `/v1`: one origin, no CORS. The `admin-api` Docker target builds the portal in |
| Sign-in | Keycloak, authorization code with PKCE (`oidc-client-ts`), public client `amaze-portal`; tokens kept in session storage. admin-api accepts tokens issued to `amaze-admin` or `amaze-portal` (`OIDC_AUDIENCE`) |
| Dev sign-in | With `APP_ENV=dev` and no `OIDC_ISSUER`, the login page lists users to pick from (`/v1/portal/dev-users`, dev only) |
| Components | Own small library (`portal/components`): cards, stats, tables, tabs, forms, SVG charts. No UI or chart framework; light and dark themes from the OS setting |
| Responsiveness | Sidebar layout on desktop, top navigation on phones (checked at 390 px: no horizontal scrolling) |

## Who sees what

| Page | Customer | Reseller | Platform |
| --- | --- | --- | --- |
| Dashboard: available balance, today's traffic, last 24 h by hour, low-balance banner | own | own | own |
| Messages: search by date, status, number, sender, reference, ID; details (live state for 96 h, then the record) | own | own | own |
| Reports: usage per day, wallet statement (charges per day; CSV with every entry), DLT report | own | own + sub-accounts | any account |
| Wallet: available, balance, credit used, in flight | own | own + sub-accounts: transfer, credit limit | any: top-up, credit limit |
| Accounts: list, create, open; suspend/reactivate; rate card; people; API keys; SMPP binds; webhook | — | sub-accounts | all accounts |
| API & SMPP: API keys (shown once), webhook (secret shown once), SMPP credentials | own (SMPP read-only) | own | own |
| Team: people who can sign in, roles | own | own | own |
| Alerts: low-balance level, e-mails, SMS numbers, webhook; recent alerts | own | own | own |
| Branding (white-label) | — | own | platform brand |
| Live monitor | — | — | ✓ |

Within an account, the user's role still limits actions: viewer and support read; finance also moves money and credit; admin and owner change everything (only an owner grants owner).

## White-label

A reseller sets its display name, logo (an upload under 200 KB is stored as a data URI, or an https URL), colour, support e-mail and optionally a portal domain.
- **Who sees it:** its sub-accounts see the reseller's brand everywhere, including alert e-mails, and never the platform's.
- **On a reseller domain:** the sign-in page is branded before anyone signs in, because the brand is found by the `Host` header.
- **Fallbacks:** direct customers see the platform's brand (set on the platform account), otherwise the built-in default.

To bring a reseller domain live:
1. The reseller points a CNAME at the portal host.
2. The platform adds a TLS certificate for it on the load balancer.
3. The platform adds `https://<domain>/callback/` as a redirect URI, and `https://<domain>/login/` as a post-logout redirect URI, to the `amaze-portal` client in Keycloak. Wildcard redirect URIs are deliberately not used: a code could be sent to any site.

## Reports

- **Usage:** per IST day: messages, parts, delivered, not delivered, messages charged to the account but sent by its sub-accounts, and the amount charged to the account.
- **Wallet statement:** opening and closing balance and totals per kind.
  - Charges, which come as one ledger entry per rater batch and so thousands a day, are folded into one row per day. Top-ups, transfers and refunds are listed one by one.
  - The CSV has every ledger entry.
  - (The first version drew every entry: 37,000 table elements for one busy day.)
- **DLT:** by template, header, PE ID or category: traffic, delivery and operator DLT failures with the main reason.
- **Messages:** newest first, 100 per page with "load more" (keyset paging on accepted time and ID), from ClickHouse.

## Live monitor (platform)

admin-api queries Prometheus (`PROMETHEUS_URL`) and the page refreshes every 10 s:
- **Rates:** accepted, refused, delivered and failed per second over 30 minutes.
- **Latency:** accept latency p99.
- **Binds:** customer binds.
- **Vendors:** per vendor, routing state (failover), binds up and submit rate.
- **Refusals and reroutes:** refusals by reason, and failover reroutes over the last 15 minutes.

Nothing is added to the message path.

## Alerts

The new `alerter` service checks every 30 s (`ALERT_EVERY`). It compares each enabled account's available amount (balance + credit limit − in flight) with its alert level.
- **Sending once:** when it drops below, the alert goes once on every configured channel, then again only after it has gone back above (a top-up re-arms it). A database claim makes sure several alerter instances send it once.
- **E-mail:** any SMTP relay (`SMTP_HOST`, `SMTP_PORT`, `SMTP_USER`, `SMTP_PASSWORD`, `SMTP_FROM`, `SMTP_TLS` = starttls / tls / none). Subject and signature carry the account's brand. In Docker Compose dev, mails land in Mailpit at http://localhost:8025.
- **SMS:** sent through the platform's own HTTP API as an alerts account (`ALERT_SMS_URL`, `ALERT_SMS_API_KEY`, `ALERT_SMS_SENDER`). The text is `ALERT_SMS_TEMPLATE`, with `{name}`, `{available}` and `{threshold}`. Register a DLT template for the alerts account that matches it, e.g. `Low balance alert for {#var#}: {#var#} available, alert level {#var#}. Please add funds.`.
- **Webhook:** HTTPS POST `{event, account_id, available, threshold, at}`, signed with the account's webhook secret (same `X-Amaze-Signature` as delivery webhooks).
- **History:** every alert is recorded with the channels that worked and the errors of those that did not, shown on the Alerts page.

## API added for the portals

| Method and path | Who |
| --- | --- |
| `GET /v1/portal/config` (public) | brand for the host, sign-in method |
| `GET /v1/portal/dev-users` | dev only |
| `GET /v1/me` | now also account name, permission and brand |
| `GET /v1/accounts[?q=&role=&parent=]`, `GET /v1/accounts/{id}`, `PUT /v1/accounts/{id}/status` | scoped by role; status needs admin on a managed account |
| `GET`/`DELETE /v1/accounts/{id}/api-keys[/{key}]`, `GET`/`DELETE …/smpp-credentials[/{cred}]`, `GET …/webhook`, `GET`/`DELETE …/users[/{user}]` | the account and its managers |
| `GET /v1/accounts/{id}/messages`, `GET …/messages/{msg}`, `GET …/dashboard`, `GET …/traffic?minutes=` | the account and its managers |
| `GET`/`PUT /v1/accounts/{id}/alerts` | read / finance |
| `GET`/`PUT /v1/accounts/{id}/branding` | resellers and platform |
| `GET /v1/monitor/live` | platform |

Changed:
- **Self-service:** accounts may now create their own API keys and set their own webhook. SMPP credentials stay with the manager, because they carry commercial TPS and bind limits.
- **Wallets:** an account may read its own wallet.

## Running it

```sh
cd portal && npm ci && npm run build && cd ..
scripts/dev-local.sh up        # admin-api serves the portal at http://localhost:8081
# optional: PROMETHEUS_BIN=/path/to/prometheus scripts/dev-local.sh up   (live monitor)
```

While working on the portal, `cd portal && npm run dev` serves it with hot reload on :3000 and proxies `/v1` to admin-api on :8081.

## Verification

- **Go tests:**
  - portal config, dev users, branding (validation, domain lookup, inheritance into `/v1/me`, one domain per brand);
  - account list scoping per role, suspend/reactivate (a suspended account cannot sign in), API key revoke, removing users (not yourself), alert validation and round trip, platform-only live monitor;
  - static serving with fallback, caching and no path escape;
  - the alerter against a fake SMTP server, SMS endpoint and webhook: once below the level, not repeated, re-armed by a top-up, a failing channel recorded while the others still go out, the reseller's brand in the e-mail.
- **Browser walkthrough** (Playwright, headless Chromium) on the dev stack with ClickHouse and Prometheus, as the platform owner, a reseller owner, the reseller's sub-account and a direct customer:
  - every page of each role renders;
  - message search, then message details;
  - all report tabs;
  - API key creation (shown once);
  - opening a sub-account;
  - the sub-account sees the reseller's brand;
  - the phone layout has no horizontal scrolling;
  - **no console errors and no failed API calls**.
- **Alerter on the dev stack:** a shop with ₹500 against an alert level of ₹1,000 fired once and recorded that e-mail was not configured.
