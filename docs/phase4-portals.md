# Phase 4: portals, reports, live monitoring, alerts, operations screens

One web app serves the admin (platform), reseller and customer portals; what each user sees follows their account's role and their own user role.
- **Slice 1:** portals, reports, live monitor, alerts, white-label.
- **Slice 2** ([below](#slice-2-dlt-commercial-and-operations-screens)): screens for everything that was API-only: DLT registrations, deals, disputes and rate cards for every role that uses them, and the platform's operations pages (vendors, routes, firewall, vendor billing, number data, error codes).

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
| DLT, deals, disputes, rate cards, operations pages | see [slice 2](#who-sees-what-slice-2) | | |

Within an account, the user's role still limits actions: viewer and support read; finance also moves money and credit; admin and owner change everything (only an owner grants owner).

## White-label

A reseller sets its display name, logo (an upload under 200 KB is stored as a data URI, or an https URL), colour and support e-mail. Its portal domain is set by the platform (since phase 5: nobody else may claim a host name).
- **Who sees it:** its sub-accounts see the reseller's brand everywhere, including alert e-mails, and never the platform's.
- **On a reseller domain:** the sign-in page is branded before anyone signs in, because the brand is found by the `Host` header.
- **Fallbacks:** direct customers see the platform's brand (set on the platform account), otherwise the built-in default.

To bring a reseller domain live:
1. The reseller points a CNAME at the portal host, and the platform sets the domain on the reseller's branding (Branding page, "Brand of" the reseller, or `PUT /v1/accounts/{reseller}/branding` as the platform).
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

## Slice 2: DLT, commercial and operations screens

### Who sees what (slice 2)

| Page | Customer | Reseller | Platform |
| --- | --- | --- | --- |
| DLT: PE IDs, headers (with active template counts), templates by header, TM chain, CSV imports (DLT portal exports as they are), message check | own | own + sub-accounts | any account; also turns DLT checks off/on per account (reason audited) |
| Deals: price deals per destination, used against the cap | own (read) | own; grants and cancels for its sub-accounts | grants and cancels for accounts directly under it; vendor volume commitments with progress and projected shortfall |
| Disputes: raise (period, amount, reason, optional message IDs), our records for the period as evidence, withdraw | raises to its provider | raises to the platform; approves or rejects its sub-accounts' claims (the credit comes from its wallet) | approves or rejects claims of accounts directly under it |
| Rate cards: prices by destination and category, current and scheduled, set one price or import CSV | — | own cards (for sub-accounts) | platform cards |
| Vendors: list with routing state (failover), binds up, sent/failed and p95 over 30 s; onboarding (SMPP binds with DLT TLV tags, or the HTTP profile); pause/activate/disable; failover policy; costs with CSV import | — | — | ✓ |
| Routes: default and per-account, least cost / priority / weighted, HLR lookup and refusal | — | — | ✓ |
| Firewall: block lists (sender, content, link domain, destination; reject or flag); pumping guard limits and blocked ranges per account, unblock | — | — | ✓ |
| Vendor billing: invoice import, reconciliation against our records with a tolerance, raise a dispute, dispute workflow (open → sent → accepted/rejected → settled) | — | — | ✓ |
| Number data: number lookup (routed network, series, cached HLR), number series (search, import, replace), MNP imports | — | — | ✓ |
| Error codes: vendor receipt codes mapped to a reason and the customer code, per vendor or for all | — | — | ✓ |

Pages a role may not use are not rendered at all (no API calls); the navigation groups the platform's pages under **Operations**.

### Onboarding a vendor from the portal

1. **Vendors → New vendor:** name, ID (names its queue and `VENDOR_ID`), SMPP or HTTP. It starts **paused**.
2. **Set it up:**
   - SMPP: add binds (host, port, TLS, system ID, password, version, bind type, window, TPS and the TLV tags the vendor wants for PE ID, template ID and TM chain). Passwords are encrypted with `SECRETS_KEY` and never shown again.
   - HTTP: the profile (base URL, auth, request template, response and receipt mapping, TPS, concurrency, secret). It is validated as the connector will load it. Give the vendor the receipt URL shown under the form.
3. **Costs:** import them (least-cost routing skips a vendor without a cost for the destination).
4. **Start a connector** with `VENDOR_ID=<id>`. Connectors read binds and the profile at start, so restart after changes.
5. **Activate** it, optionally after setting a failover policy. Add routes if it should not only be chosen by least cost.

### API added for slice 2

| Method and path | Notes |
| --- | --- |
| `GET`/`POST /v1/vendors`, `GET /v1/vendors/{vendor}` | list with routing state; create (paused); detail with binds (no passwords), HTTP profile (`dlr_path`, `has_secret`), routing and connector reports |
| `PUT /v1/vendors/{vendor}/status` | `active`, `paused`, `disabled` |
| `POST /v1/vendors/{vendor}/binds`, `DELETE …/binds/{bind}` | SMPP vendors; password sealed; `tlv_profile` keys `pe_id`, `template_id`, `tm_chain` as hex tags |
| `PUT /v1/vendors/{vendor}/http-profile` | HTTP vendors; omitting `secret` keeps the stored one |
| `GET /v1/vendors/{vendor}/costs` | each row `current`, `scheduled` or `superseded` |
| `GET /v1/vendor-invoices[?vendor=]` | with the latest dispute status |
| `GET /v1/accounts/{id}/firewall-limits` | effective limits and whether they are custom; the account may read its own |
| `GET /v1/accounts/{id}/dlt/entities/{pe}/headers` | headers with their active template count |
| `GET /v1/number-series[?q=]`, `GET /v1/mnp/imports` | series search (first 500) with the total; MNP import history |

All are platform only, except the firewall limits and DLT headers reads, which follow the account rules (own or managed).

Fixed along the way:
- Disputes now carry `account_name`.
- `GET /v1/numbers/{msisdn}` returns `mcc`, `mnc`, `source` in lower case.
- Importing a vendor invoice without ClickHouse returned the error body and the success body in one response; it now returns only the success (the invoice is stored, reconciliation `unavailable`).
- Binds and HTTP profiles are validated up front (SMPP version, bind type, window, TPS, base URL, required mappings) instead of failing on database constraints.

### Verification (slice 2)

- **Go tests** (`TestOpsEndpoints`): vendor create and validation, platform-only access, bind validation (port, password length, SMPP version, TLV tags), the password stored sealed and never returned, bind disable once, HTTP profile validation and the secret kept on a later save, cost states, invoice import without ClickHouse giving one clean response, firewall limit defaults and custom values, DLT headers with template counts, series search.
- **Browser walkthrough** (Playwright, headless Chromium) on the dev stack with ClickHouse and Prometheus:
  - **Customer:** DLT check of the demo OTP (accepted, with template, PE ID and TM chain) and of other text (refused `dlt_template`, SMPP `0x412`, receipt 603); templates tab; raising a dispute.
  - **Reseller sub-account and its reseller:** the sub-account raises a dispute; the reseller approves ₹2 of ₹3; the reseller grants a capped deal to its sub-account.
  - **Platform:**
    - onboards an SMPP vendor (bind with TLV tags, cost import) and opens the HTTP simulator's profile;
    - a bad TLV tag is refused with the server's message;
    - adds a priority route and a content rule;
    - looks up a number, imports series and error codes;
    - creates a vendor commitment and opens its progress;
    - imports an invoice that over-bills (₹70 billed against ₹25 in our records), disputes ₹45 and moves the dispute to sent;
    - rejects the customer's dispute.
  - **Access and layout:** pages outside a role show "not available" without any API call; no console errors and no failed API calls (other than the one deliberate refusal); vendor, DLT and vendor billing pages have no horizontal scrolling at 390 px.
