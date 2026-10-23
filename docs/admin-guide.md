# Portal user guide

This guide is for the people who run the web portal day to day:
- platform operations staff;
- resellers;
- customer account admins.

It describes what each page shows and does, who may use it, and what its error messages mean. It covers the portal only. The customer HTTP API is in `api/openapi.yaml`. How the platform is built is in the other files under `docs/`.

Amounts are in Indian rupees (₹). Times are shown in IST, except on `Messages` and `Reports`, which use the time zone you pick there (UTC unless you change it; see [Time zone](#time-zone)).

## Contents

1. [Introduction](#1-introduction)
2. [Pages](#2-pages)
3. [Common workflows](#3-common-workflows)
4. [Troubleshooting and FAQ](#4-troubleshooting-and-faq)
5. [Glossary](#5-glossary)

---

## 1. Introduction

### What the portal is

One web app serves three kinds of account: the platform, resellers and customers. Everybody signs in at the same address, or at a reseller's own domain. What you see depends on two things:
- your **account's role** (platform, reseller or customer);
- your **own user role** within that account (viewer, support, finance, admin or owner).

Pages your account may not use are left out of the navigation. If you open one by its address anyway, you see `This page is not available for your account.`

### The account tree

```
Platform
├── Direct customer            (customer, parent = platform)
├── Direct customer
└── Reseller                   (reseller, parent = platform)
    ├── Sub-account            (customer, parent = reseller)
    └── Sub-account
```

| Account role | Who it is | Manages |
| --- | --- | --- |
| `platform` | The company running the platform | Every account |
| `reseller` | A business that resells SMS under its own brand | Its own account and its direct sub-accounts |
| `customer` | A sender: either a direct customer of the platform, or a sub-account of a reseller | Only itself |

Some rules follow from the tree:
- Only an account's **parent** grants it deals and resolves its disputes.
- The platform books **top-ups** (payments received). A reseller funds its sub-accounts by **transfer** from its own wallet.
- A reseller pays the platform's price for every message its sub-accounts send. Its sub-accounts pay the reseller's price.

### User roles and permission levels

Each person who can sign in belongs to one account and has one role there. The role maps to a permission level:

| User role | Permission level | May |
| --- | --- | --- |
| `viewer` | read | Look at everything the account may see |
| `support` | read | Same as viewer |
| `finance` | finance | Read, plus move money and credit: top-ups, transfers, credit limits, alert settings, disputes, vendor invoices |
| `admin` | admin | Everything, including finance actions |
| `owner` | admin | Everything. Only an owner can grant or remove the owner role |

Each level includes the ones below it. The portal shows your account and your role at the bottom of the navigation, for example `ops@example.com · finance`.

If you try something your role does not allow, the message is `your role (<role>) does not allow this`. Most buttons are hidden from people who cannot use them, so you will rarely see it.

### Signing in

The portal uses single sign-on (Keycloak).

1. Open the portal address.
2. Click `Sign in`. You go to the sign-in page, which handles your password, one-time code and lockout.
3. After signing in you return to the page you asked for.

To sign in, your e-mail must:
- exist in Keycloak and be verified;
- be added as a person on an account in the portal (see [Team](#team));
- belong to an account that is active (not suspended).

If any of these is missing, the portal shows `signed in, but no platform user for this email`. When your session ends, you see `Your session ended. Please sign in again.` and go back to the sign-in page.

`Sign out` is at the bottom of the navigation. If your brand has a support e-mail, a `Support` link is shown next to it.

### Branding and own domains

- **Direct customers** see the platform's brand. If the platform has not set one, they see the built-in default.
- **Resellers' sub-accounts** see the reseller's brand everywhere, including alert e-mails. They never see the platform's brand.
- **A reseller's own domain** brands the sign-in page as well, before anyone signs in.

Only the platform can set a portal domain. See [Branding](#branding) and [Onboarding a reseller](#onboarding-a-reseller-with-its-own-brand-and-domain).

---

## 2. Pages

Pages are listed in navigation order. The navigation groups some pages under **Settings** and the platform's own pages under **Operations**.

### Who sees which page

| Page | Customer | Reseller | Platform |
| --- | --- | --- | --- |
| Dashboard | ✓ | ✓ | ✓ |
| Messages | ✓ | ✓ | ✓ |
| Reports | own | own + sub-accounts | any account |
| Wallet | own | own + sub-accounts | any account |
| Accounts | — | sub-accounts | all accounts |
| Live monitor | — | — | ✓ |
| DLT | own | own + sub-accounts | any account |
| Deals | own (read only) | own; grants to sub-accounts | grants to accounts directly under it; vendor commitments |
| Disputes | raises its own | raises its own; resolves its sub-accounts' | resolves its direct accounts' |
| Rate cards | — | own cards | platform cards |
| API & SMPP (Settings) | ✓ | ✓ | ✓ |
| Team (Settings) | ✓ | ✓ | ✓ |
| Alerts (Settings) | ✓ | ✓ | ✓ |
| Branding (Settings) | — | own | platform and resellers |
| Vendors, Routes, Firewall, Vendor billing, Number data, Error codes (Operations) | — | — | ✓ |

### Who may do what

| Action | Account role | Permission |
| --- | --- | --- |
| Create an account | platform, reseller | admin |
| Suspend or reactivate an account | platform (any), reseller (its sub-accounts) | admin |
| Change an account's rate card | platform, reseller | admin |
| Record a top-up | platform | finance |
| Transfer to a sub-account | reseller | finance |
| Change a credit limit | platform (any), reseller (its sub-accounts) | finance |
| Add or remove people | own account, or one you manage | admin |
| Create or revoke API keys | own account, or one you manage | admin |
| Create or disable SMPP credentials | platform, reseller (for accounts they manage) | admin |
| Set the delivery receipt webhook | own account, or one you manage | admin |
| Save alert settings | own account | finance |
| Edit branding | reseller (own), platform (own and resellers') | admin |
| Set a portal domain | platform | admin |
| Add PE IDs, import headers and templates, set TM chains | own account, or one you manage | admin |
| Run a DLT message check | anyone | read |
| Turn DLT checks off or on for an account | platform | admin |
| Grant or cancel a deal | the account's direct parent | admin |
| Create or cancel a vendor commitment | platform | admin |
| Raise or withdraw a dispute | the account itself (not the platform) | finance |
| Resolve a dispute | the account's direct parent | finance |
| Create rate cards and set prices | platform, reseller (own cards) | admin |
| Vendors, routes, firewall, number data, error codes: changes | platform | admin |
| Vendor invoices: import, dispute, update | platform | finance |

Every change (anything that is not a read) is written to an audit log with your e-mail, your account, what you changed, the result and your IP address.

---

### Dashboard

**Who:** everyone. It shows your own account.

The page title is `Welcome, <account name>`. It refreshes every 30 seconds.

**Wallet tiles:**

| Tile | Meaning |
| --- | --- |
| `Available to send` | Balance + credit limit − in flight. Messages are refused once this reaches zero. Red at or below zero |
| `Balance` | Your balance. If you are using credit, the hint shows `credit used ₹…` |
| `Credit limit` | How far below zero your balance may go |
| `In flight` | Money held for messages accepted but not yet charged |

**Today's traffic** (since midnight IST):

| Tile | Meaning |
| --- | --- |
| `Sent today` | Messages accepted today; the hint shows what they were charged |
| `Delivered` | Messages with a delivered receipt, and the share of all sent |
| `Failed` | Failed, expired and rejected messages together |
| `Awaiting receipt` | Accepted or submitted messages with no final receipt yet |

**Last 24 hours:** a bar chart per hour (delivered, failed, awaiting receipt), with a link to `Reports`. If the message store is not running, the card says `Traffic charts need the message store (ClickHouse).`

**Low-balance banner:** if you have set an alert level (see [Alerts](#alerts)) and the available amount is below it, a banner says so.

---

### Messages

**Who:** everyone. It searches the **signed-in account's own** messages only. There is no account picker on this page.

#### To search for messages

1. Open `Messages`.
2. Fill in any of the filters:

   | Field | Meaning |
   | --- | --- |
   | `From` | First day to search, from midnight in the chosen time zone. Defaults to yesterday |
   | `To (exclusive)` | Day to stop at; that day itself is not included. Leave empty for "up to now" |
   | `Time zone` | The zone for the dates and the times shown. See [Time zone](#time-zone) |
   | `Status` | `accepted`, `submitted`, `delivered`, `failed`, `expired`, `rejected`, or `Any` |
   | `Mobile number` | The destination, e.g. `98XXXXXXXX` (with or without country code) |
   | `Sender` | The header the message was sent with, exact |
   | `Your reference` | The client reference sent with the message, exact |
   | `Message ID` | The platform's message ID |

3. Click `Search`.

Results are newest first, 100 at a time. Click `Load more` for the next 100. Click a row to open the message.

**Result columns:** `Sent` (in the chosen zone, for example `Sent (IST)`), `To`, `Sender`, `Status`, `Error`, `Parts`, `Charged`, `Template`, `Reference`.

#### Message statuses

| Status | Meaning |
| --- | --- |
| `accepted` | We accepted it and held the money; not yet with a vendor |
| `submitted` | A vendor accepted it; waiting for the delivery receipt |
| `delivered` | The receipt says delivered |
| `failed` | The receipt says it failed |
| `expired` | No final receipt arrived in time |
| `rejected` | Refused by the platform after it was accepted (for example, no route) |

Messages **refused at submit** (DLT or firewall checks, no balance) are never stored. The sender gets the refusal straight away (HTTP 422 or an SMPP error status), and they do not appear here.

#### Message detail

The card title says `Live state` while the message is still in the live store (about 96 hours), and `Message record` after that.

| Field | Meaning |
| --- | --- |
| `Message ID` | The platform's ID |
| `Status`, `Error` | Current status and error code (see [Reading an error code](#reading-an-error-code)) |
| `To`, `Sender`, `Parts` | Destination, header, number of SMS parts |
| `Price` | Price per message as charged to this account |
| `Accepted`, `Completed` | When we accepted it, and when it reached a final status, in the time zone last chosen on `Messages` or `Reports` (shown in the label, for example `Accepted (IST)`) |
| `Network (MCC/MNC)` | The network the number was resolved to |
| `DLT PE ID`, `DLT template`, `DLT category` | The DLT fields sent to the operator |
| `Vendor`, `Vendor message ID` | Who carried it, and their ID for it |
| `Your reference` | The client reference |
| `Channel` | How it came in (HTTP or SMPP) |

Tip for platform and reseller staff: to open a message of an account you manage, use the address `/messages/view/?id=<message ID>&account=<account ID>`. The account ID is on the account's `Overview` tab. Links in dispute evidence open messages this way.

---

### Reports

**Who:** everyone. Platform and reseller users get an `Account` picker (their own account, or one they manage).

There are three tabs: `Usage`, `Wallet statement` and `DLT`. Each has `From`, `To (exclusive)`, `Time zone` and `Show`. The default range is the last 30 days up to and including today. Changing the time zone reloads the tab.

#### Time zone

The `Time zone` selector on `Messages` and `Reports` sets how dates are read and times are shown:
- Choices: `UTC` (the default), `India (IST)`, `Dubai (GST)`, `Singapore (SGT)`, `London`, `New York`, and your device's own zone (marked `(this device)`).
- Your choice is remembered in this browser, and used on both pages and on the message detail page.
- `From` and `To` mean midnight in that zone. For example, with `India (IST)`, `From` 1 March covers from 00:00 IST on 1 March.
- Times in tables carry the zone in the column title, for example `Day (IST)` or `When (UTC)`.
- Usage days are grouped by days in that zone, and statement times (on screen and in the CSV) are in that zone.

Other pages show times in IST.

#### Usage

Per day (`Day`, in the chosen zone): `Messages`, `Parts`, `Delivered`, `Not delivered`, `Via sub-accounts`, `Charged`. Totals are shown above a bar chart.
- `Via sub-accounts` counts messages your sub-accounts sent that were charged to you (resellers).
- `Charged` is what this account was charged, as sender or as reseller.

Needs the message store. Without it the error is `usage reports need CLICKHOUSE_URL`.

#### Wallet statement

Shows the `Opening balance`, `Closing balance` and a total per entry kind (for example charge, topup, transfer, refund).

The table lists top-ups, transfers and refunds one by one. Charges come in batches, thousands a day, so the table folds them into one row per day in the chosen zone (`charges on <date>`).

To download every ledger entry:
1. Pick the account and the period.
2. Click `Download CSV`.

The CSV has the columns `at, kind, amount, balance_after, reference, messages, counterparty`. `at` is in the chosen time zone (with its offset, for example `+05:30`). Text that would start with `=`, `+`, `-` or `@` is prefixed with `'`, so a spreadsheet does not run it as a formula.

A statement may hold at most 100,000 entries. Beyond that the error is `more than 100,000 entries; use a shorter period`.

#### DLT

Traffic grouped by `Template`, `Header`, `PE ID` or `Category` (choose with `Group by`). Columns: `Messages`, `Delivered`, `Not delivered`, `Operator DLT failures`, `Main DLT reason`.

This report covers only failures reported by operators. Messages refused at submit for DLT never reach operators, and are not counted here.

---

### Wallet

**Who:** everyone. Platform and reseller users get an `Account` picker. Customers see only their own wallet.

**Tiles:**

| Tile | Meaning |
| --- | --- |
| `Available to send` | Balance + credit limit − in flight |
| `Balance` | Live balance. The hint `settled ₹…` is the balance in the ledger, which catches up as charges are settled in batches |
| `Credit limit` | The hint shows credit used, or `none used` |
| `In flight` | Held for messages not yet charged |

The `Statement` link at the top goes to [Reports](#reports).

On your own wallet, resellers and customers see: `To add funds, pay … and your balance is credited here. Credit limits are set by your provider.`

#### Rules

- Every new account starts **prepaid**: credit limit ₹0.
- An account can send while `balance + credit limit − in flight` covers the message.
- A top-up adds to the balance, so credit used is repaid first.
- A reseller's sub-account credit limits may add up to at most the reseller's balance + its own credit limit.
- Lowering a limit below what is already used is allowed. New messages are refused until a top-up brings the available amount above zero.

#### To record a top-up (platform)

Use this when the platform has received a payment.

1. Open `Wallet` and choose the account in `Account` (or open it from `Accounts` and click `Wallet`).
2. In `Record a payment (top-up)`, enter:
   - `Amount (₹)`: more than zero;
   - `Payment reference`: the UTR or invoice number (required).
3. Click `Add funds`. The page says `Top-up booked`.

The form protects against booking the same payment twice: if you click again after an error, it reuses the same request key. If the first attempt did go through, the second gets `top-up with this Idempotency-Key already booked`.

Only the platform books top-ups. Others get `only the platform books top-ups`.

#### To transfer to a sub-account (reseller)

1. Open `Wallet` and choose the sub-account.
2. In `Transfer from your wallet`, enter the `Amount (₹)` and, optionally, a `Reference`.
3. Click `Transfer`. The page says `Transferred`.

If your own available amount does not cover it: `your available balance does not cover this transfer`.

#### To change a credit limit (platform or reseller)

1. Open `Wallet` and choose the account. (You cannot change your own limit here.)
2. In `Credit limit`, enter:
   - `New limit (₹)`: 0 means prepaid only;
   - `Reason (audited)`: required.
3. Click `Save`. The page says `Credit limit changed`. It applies to the next message.

| Error | Meaning |
| --- | --- |
| `wallet: credit limit must be >= 0` | Negative limits are not allowed |
| `credit: reseller available balance does not cover sub-account credit limits…` | The reseller's sub-account limits would add up to more than its balance + credit limit |
| `credit: actor may not change this account's credit limit` | A reseller may change only its direct sub-accounts |
| `credit: actor and reason are required` | The reason was empty |

---

### Accounts

**Who:** platform and reseller users. Resellers see the page as `Sub-accounts`.

The list shows `Name`, `Type`, `Under` (platform only), `Status`, `Available`, `Balance`, `Credit`, `Rate card` and `Created`. Use `Search` to filter by name. The platform can also filter by `Role` (`Resellers` or `Customers`). Up to 500 accounts are listed. Click a row to open the account.

#### To create an account

Needs admin.

1. Click `New account`.
2. Fill in:

   | Field | Meaning |
   | --- | --- |
   | `Name` | Up to 120 characters, no line breaks |
   | `Type` | Platform only: `Customer` or `Reseller`. Resellers always create customers (sub-accounts) |
   | `Rate card` | The prices this account pays. You choose from your own cards: platform cards for the platform, the reseller's own cards for a reseller |

3. Click `Create`. The new account opens.

New accounts start prepaid (credit limit 0). Next, add funds and people. A reseller with no rate card of its own has nothing to choose in `Rate card`: create one first (see [Rate cards](#rate-cards)).

Errors:
- `name and role (customer or reseller) are required`
- `rate card not found among yours`

#### The account page

The account page has three tabs. At the top are the `Wallet` link and the `Suspend` or `Reactivate` button.

**`Overview`**
- Tiles: `Available`, `Balance`, `Credit limit`, `Status`.
- `Details`: `Type` (with the number of sub-accounts), `Under`, `Rate card`, `DLT checks` (`enforced`, or `off`) and `Account ID`.
- `Change rate card` (admins): pick one of your cards and click `Save`. Prices apply within a minute.
- `Usage and statement`: opens Reports for this account.

**`People`**: the same as [Team](#team), for this account.

**`API & SMPP`**: `SMPP binds`, `HTTP API keys` and `Delivery receipt webhook`, for this account. See [API & SMPP](#api--smpp).

#### To suspend or reactivate an account

Needs admin.

1. Open the account.
2. Click `Suspend` (or `Reactivate`) and confirm.

What suspension does:
- The account stops sending within a minute.
- Its people can no longer sign in.

Rules:
- You cannot suspend your own account: `you cannot suspend your own account`.
- A suspension can be lifted only by whoever imposed it, or by the platform. If the platform suspended a reseller's sub-account, the reseller gets `this account was suspended by the platform; only the platform can reactivate it`.

---

### Live monitor

**Who:** platform users. It refreshes every 10 seconds.

| Part | Shows |
| --- | --- |
| Tiles | `Accepted`, `Refused` and `Delivered` per second; `Accept p99` latency, with the number of customer binds |
| `Throughput, last 30 minutes` | Accepted, delivered, failed and refused per second |
| `Vendors` | Per vendor: `Routing` (up or down), `Binds up`, `Submits /s` |
| `Submit rate by vendor` | One line per vendor |
| `Refusals, last 5 minutes` | Refusals per second by reason code (for example `dlt_template`, `fw_series`) |
| `Failover reroutes, last 15 minutes` | Messages moved off a vendor, by vendor and reason |

It needs Prometheus. Without it the errors are `live monitoring needs PROMETHEUS_URL` or `monitoring store unavailable`.

---

### DLT

**Who:** everyone. Platform and reseller users get an `Account` picker.

In India every commercial SMS must match the sender's DLT registrations. The platform checks every message against these registrations at submit. A message that operator scrubbing would reject is refused straight away, with a reason. This page keeps the registrations the platform checks against.

#### Principal entities (PE IDs)

The table lists each PE ID with its `Name`, `Status`, `Headers`, `Active templates` and `TM chain`. Click a row to select it.

##### To add or update a PE ID

Needs admin.

1. Click `Add entity`.
2. Fill in:

   | Field | Meaning |
   | --- | --- |
   | `PE ID` | As registered on the operator's DLT portal: 10–30 digits |
   | `Entity name` | Required |
   | `TM chain` | Optional: the PE–TM chain value registered on the DLT portal |

3. Click `Save`. Edges apply the change within a minute.

A PE ID belongs to one account only. If someone else registered it first: `this PE ID is registered to another account`.

#### The selected entity

The selected entity has four tabs.

**`Headers`**: each header with its `Category`, `Status` and `Active templates`. Click a header to see its templates.

**`Templates`**: filter by `Header`. Shows `Template ID`, `Name`, `Category`, `Status`, `Headers`, `Body` and `Whitelisted` (allowed URLs, APKs and callback numbers).

**`TM chain`**: the chain sent with every message of this PE ID that does not carry its own. Without one, messages are refused (`dlt_tm_chain`). Admins edit it and click `Save`. The old value is kept for history.

**`Import`** (admins): upload CSV files exported from the operator DLT portal. You can upload them as they are; save Excel files as CSV first.

##### To import headers and templates

Import headers first. Templates can only use headers already registered under the same PE ID.

1. Select the PE ID and open `Import`.
2. Under `Headers`, choose the `CSV file` (or paste rows) and click `Import`. Columns: `header, category[, status]`.
3. Under `Content templates`, do the same. Columns: `template_id, category, header, body[, name, status, consent, urls, apks, callbacks]`.
4. Check the result: `Imported N rows.`

Imports are all or nothing. If any line is wrong, nothing is imported, and every bad line is listed.

| Rule | Error |
| --- | --- |
| Headers are 6 letters. Promotional headers are 6 digits | `header "…" must be 6 letters (6 digits if promotional)` |
| Category is Transactional, Service Implicit, Service Explicit or Promotional. `Others` counts as service implicit | `unknown category "…"` |
| Status: Active (also approved, enabled), or suspended (also inactive, deactivated, blocked, disabled, rejected) | `unknown status "…"` |
| A service explicit template needs a consent template ID | `service explicit template needs a consent template ID` |
| A template's headers must be registered under this PE ID | `template …: header … is not registered under this PE ID` |
| Promotional templates only on promotional headers, and the others only on non-promotional headers | `template …: … template cannot use … header …` |
| A template ID belongs to one PE ID | `template … is registered under another PE ID` |

Re-importing a template replaces its headers and whitelist. To suspend a template, re-import it with status `Inactive`.

#### Check a message

Needs only read access. This runs the same checks as a real submit, without sending anything.

1. Fill in `Sender (header)` and `Text`. Optionally choose a `PE ID` (default `From the header`), and add a `Template ID` (when empty, it is matched by text) or a `TM chain`.
2. Click `Check`.

The result is either:
- `would be accepted`, with the matched template and category, the PE ID and the TM chain sent; or
- `would be refused`, with the reason code, why (for example `header ABCDEF is not registered`), and the SMPP status and receipt code the sender would get.

The checks, in order (the first failure is the reason):

| Reason | Meaning | SMPP status | Receipt code |
| --- | --- | --- | --- |
| `dlt_entity` | The PE ID given is not registered to this account, or not active | 0x410 | 601 |
| `dlt_header` | The sender is not a registered, active header | 0x411 | 602 |
| `dlt_template` | No active template of this header matches the text (or the given template ID does not) | 0x412 | 603 |
| `dlt_category` | Promotional template on a non-promotional header, or the reverse | 0x413 | 604 |
| `dlt_promo_hours` | Promotional message outside 09:00–21:00 IST | 0x414 | 605 |
| `dlt_consent` | Service explicit template without a consent template | 0x415 | 606 |
| `dlt_url`, `dlt_callback` | A link or callback number in the variable parts is not whitelisted on the template | 0x417, 0x418 | 608, 609 |
| `dlt_tm_chain` | No TM chain from the sender and none registered for the PE ID | 0x416 | 607 |

Template matching: the fixed text must match exactly, including case; runs of spaces and line breaks count as one. `{#var#}` matches up to 30 characters.

#### DLT checks on or off (platform)

A platform admin who picks another account sees a `DLT checks` card:
- To switch checks off, enter `Reason (audited)` and click `Turn checks off`.
- To switch them back on, click `Enforce checks`.

When checks are off, everyone viewing that account sees the card with `off for this account`. Operators still scrub the messages, so unregistered traffic then fails at the operator instead. Use this only for cases such as a test account.

---

### Deals

**Who:** everyone. The platform has two tabs: `Price deals for accounts` and `Vendor volume commitments`.

#### Price deals for accounts

A deal is a special price for one account on one destination for a period, optionally for a capped number of parts. While it runs, it replaces the rate card price. After the cap is used up, the rate card applies again.

The table shows `Deal`, `Destination (MCC/MNC)`, `Price / part`, `Used / cap`, `Runs` and `State` (`scheduled`, `running`, `ended`, `cancelled`).

##### To grant a deal

Needs admin, and you must be the account's direct parent:
- the platform grants deals to its direct customers and resellers;
- a reseller grants deals to its sub-accounts.

1. Open `Deals` and choose the account in `Account`.
2. In `Grant a deal`, fill in:

   | Field | Meaning |
   | --- | --- |
   | `Name` | Required |
   | `MCC` | 3 digits; `404` by default |
   | `MNC` | 2–3 digits, or `*` for every network |
   | `Price per part (₹)` | Zero or more |
   | `Cap (parts)` | Optional; empty means no cap |
   | `From (IST)` | Start day. If it is today, the deal starts now |
   | `To (IST, inclusive)` | Last day; must be after the start and in the future |

3. Click `Grant`. The page says `Deal granted`.

Rules:
- Two active deals for the same account and destination may not overlap in time: `overlaps deal "…" on the same destination`.
- A deal for an exact network beats a `*` deal.
- A message that does not fit entirely in the remaining cap is priced at the card rate as a whole.
- Changes, including cancellation, take effect within a minute.

To cancel, click `Cancel` on a running or scheduled deal and confirm. Prices go back to the rate card within a minute.

The grant form and `Cancel` button appear for any account you can pick. The server still allows only the direct parent. For example, the platform picking a reseller's sub-account gets `only the account's parent (platform or reseller) can do this`. Cancelling a deal someone else granted gets `no active deal you granted with this id`.

#### Vendor volume commitments (platform)

Volume bought from a vendor at a lower cost for a period. While the commitment is unmet, least-cost routing picks that vendor first on that destination. The vendor may charge the shortfall rate for each part we fall short.

##### To add a commitment

Needs admin.

1. Click `New commitment`.
2. Fill in `Vendor`, `Name`, `MCC`, `MNC`, `Cost per part (₹)`, `Committed parts`, `Shortfall rate (₹/part)`, `From (IST)` and `To (IST, inclusive)`.
3. Click `Create`. Routing picks it up within 15 seconds.

Click a commitment row to see its progress:
- `Met`: percent of parts billed;
- `Still needed`: parts still needed, per day, for the days left;
- `Billed cost`;
- `Projected shortfall`: shown after the first full day, with the projected charge.

Admins can click `Cancel commitment`.

---

### Disputes

**Who:** everyone.
- **Customers** raise claims to their provider (the reseller, or the platform for direct customers).
- **Resellers** raise their own claims to the platform, and resolve their sub-accounts' claims. Approving pays the credit from the reseller's wallet.
- **The platform** resolves claims from accounts directly under it. It does not raise disputes.

The list shows `Raised`, whether a dispute is `yours` or `to resolve`, `Account`, `Period`, `Claimed`, `Credited`, `Status` and `Reason`. Filter by `Status`: `Open`, `Approved`, `Rejected` or `Withdrawn`.

#### To raise a dispute

Needs finance.

1. Click `Raise a dispute`.
2. Fill in:

   | Field | Meaning |
   | --- | --- |
   | `From (IST)`, `To (IST, inclusive)` | The period the claim covers |
   | `Amount claimed (₹)` | More than zero |
   | `What is wrong` | Required |
   | `Message IDs` | Optional, up to 1,000, one per line or comma-separated (copy them from Messages) |

3. Click `Raise`. The dispute opens.

To withdraw an open dispute of yours, open it and click `Withdraw` (needs finance).

#### To resolve a dispute

Needs finance, and you must be the account's parent.

1. Open the dispute. `Our records for the period` shows the evidence:
   - the account's messages in the period by final status, with parts and charges;
   - for each listed message ID: its status, error, parts and charge. The platform also sees the vendor;
   - IDs with no record for this account, listed separately.
2. Under `Decide`, choose `Approve and credit` or `Reject`.
3. To approve, set `Credit (₹)`: more than zero, up to the claimed amount.
4. Enter a `Note to the account` (required) and click `Approve` or `Reject`.

The credit is paid at once:
- **Platform:** it is a refund to the account.
- **Reseller:** it moves from the reseller's wallet to the sub-account. If the reseller's available amount does not cover it, you get `your available balance does not cover this credit`, and the dispute stays open.

A dispute can be paid only once. A second decision gets `dispute is not open`.

---

### Rate cards

**Who:** platform and reseller users. The platform keeps platform cards (for its direct customers and resellers). A reseller keeps its own cards (for its sub-accounts).

A rate card holds the price per part, by destination and DLT category:
- **Destination:** MCC, plus MNC or `*` for the whole country.
- **Category:** `transactional`, `service implicit`, `service explicit`, `promotional`, or `any`.

An entry for an exact network beats a `*` entry. A future date schedules a price change. Deals override the card while they run.

Pick a card in `Rate card`. Use `Find` to filter by `404/45` or by category. Each row shows `MCC`, `MNC`, `Category`, `Price / part`, `From`, and whether it is `current` or `scheduled`.

#### To create a card and set prices

Needs admin.

1. Under `New rate card`, enter a `Name` and click `Create`.
2. Under `Set a price`, fill in `MCC`, `MNC`, `Category`, `Price per part (₹)` and, optionally, `From` (empty means now).
3. Click `Save`. The page says either `Price set; applies within a minute` or `Price change scheduled`.

#### To import prices

Under `Import prices`, upload a CSV. A header row names the columns, in any order:
- `mcc` and `price` are required;
- `mnc`, `category` and `valid_from` (RFC 3339, e.g. `2026-11-01T00:00:00+05:30`) are optional.

Rules:
- 1 to 50,000 rows.
- MCC is 3 digits. MNC is 2–3 digits or `*`.
- Price is zero or more, with at most 4 decimals.
- A bad row stops the import with `row N: …` or `csv line N: …`.

The portal has no way to delete a price. Set a new one instead.

---

### API & SMPP

**Who:** everyone, for their own account. The same three cards appear on an account's `API & SMPP` tab for accounts you manage.

The `Sending` card summarises the HTTP request (`POST /v1/sms` with header `X-API-Key`) and the SMPP DLT tags:
- `0x1400`: PE ID;
- `0x1401`: template ID;
- `0x1402`: TM chain.

#### HTTP API keys

Lists keys by `Name`, `Starts with` (prefix), `Created` and `State`.

To create a key (needs admin):
1. Enter a `Name`, for example `production server`.
2. Click `Create key`.
3. Copy the key now. It is shown only once (`shown once, store it now`).

To revoke a key, click `Revoke` and confirm. Requests with it are refused within a minute.

#### SMPP binds

Lists credentials by `system_id`, `TPS / bind`, `Max binds`, `Allowed IPs` and `State`.

SMPP credentials carry commercial TPS and bind limits, so only the manager creates them:
- the platform, for any account;
- a reseller, for its sub-accounts.

On their own `API & SMPP` page, resellers and customers can only read them (`No SMPP credentials. Ask your provider for one.`).

To create one (needs admin):
1. Fill in:

   | Field | Rule |
   | --- | --- |
   | `system_id` | 1–15 characters |
   | `Password` | 6–8 characters |
   | `TPS per bind` | Default 100 |
   | `Max binds` | Default 4 |
   | `Allowed IPs` | IPs or CIDRs, comma-separated; empty means any |

2. Click `Create`.

If the system ID or password is outside these limits, the error is `system_id (1-15 chars) and password (6-8 chars) are required`.

To disable a credential, click `Disable` and confirm. New binds are refused. Binds already open end at their next re-bind.

#### Delivery receipt webhook

The final status of every HTTP message is POSTed to this URL, signed with `X-Amaze-Signature` (HMAC-SHA256).

To set it (needs admin):
1. Enter the URL in `HTTPS URL`.
2. Click `Save`.
3. Copy the `Signing secret`. It is shown only once.

Every save creates a new secret. To remove the webhook, empty the field and click `Remove`.

---

### Team

**Who:** everyone, for their own account. The `People` tab of an account shows the same for accounts you manage.

`People who can sign in` lists each person's `E-mail`, `Role`, `Last sign-in` and `State`.

#### To add a person

Needs admin.

1. Enter their `E-mail` and choose a `Role`: `viewer`, `support`, `finance` or `admin`. Owners also see `owner`.
2. Click `Add person`.

The page says `Added. They sign in with single sign-on using this e-mail.` The person also needs a Keycloak login with this e-mail, verified.

E-mails are unique across the whole platform. An e-mail already used on any account gets `already exists`.

#### To remove a person

Click `Remove` and confirm. Rules:
- You cannot remove yourself: `you cannot remove yourself`.
- Only an owner can remove an owner: `only an owner can remove an owner`.

---

### Alerts

**Who:** everyone, for their own account. Saving needs finance.

The low-balance alert compares the account's available amount with a level. When it drops below:
- the alert goes out once on every channel you set;
- it goes out again only after the amount has gone back above the level (a top-up re-arms it).

The platform checks every 30 seconds.

#### To set up the alert

1. Fill in:

   | Field | Meaning |
   | --- | --- |
   | `Alert below (₹)` | The level, zero or more |
   | `E-mail to` | Comma-separated addresses |
   | `SMS to` | Indian mobile numbers, comma-separated |
   | `Webhook (HTTPS)` | Gets a POST with `{event, account_id, available, threshold}`, signed like delivery webhooks |
   | `Enabled` | Turns the alert on or off |

2. Click `Save`.

The card header shows `armed`, or `below level now` while the alert is active.

| Error | Meaning |
| --- | --- |
| `"…" is not an e-mail address` | Fix the address |
| `"…" is not an Indian mobile number` | Fix the number |
| `webhook_url must be an https URL` | Only https is allowed |
| `add at least one e-mail, SMS number or webhook` | An enabled alert needs a channel |

`Recent alerts` lists the last 20: `When`, `Event`, `Available`, `Level`, `Sent by` (the channels that worked) and `Problems` (errors from those that did not).

---

### Branding

**Who:** platform and reseller users. Saving needs admin.

**Reseller:** edits its own brand. Its customers see this name, logo and colour, on its own domain if it has one.

**Platform:**
- `Brand of` chooses the platform's own brand or a reseller's.
- The platform brand is shown on the main domain and to accounts without a reseller brand.

| Field | Rule |
| --- | --- |
| `Display name` | Required; up to 60 characters, no line breaks |
| `Logo` | An image file (PNG, SVG, JPEG or WebP) under 45 KB. It is stored inside the brand |
| `Colour` | Used for buttons and highlights |
| `Support e-mail` | Shown as a `Support` link and on the sign-in page |
| `Portal domain` | Platform only. Resellers see it read-only |

The `Preview` card shows how the sign-in page will look. Click `Save` to apply.

The logo is stored inline with the brand, so it must be under 45 KB. Use an optimised PNG or SVG.

| Error | Meaning |
| --- | --- |
| `name is required (up to 60 characters, no line breaks)` | Fix the display name |
| `primary_color must look like #1f6feb` | Colour format |
| `support_email is not an e-mail address` | Fix the support e-mail |
| `the portal domain is set by the platform; ask it to add yours` | A reseller tried to change the domain |
| `already exists` | Another brand already uses this domain |

---

### Vendors

**Who:** platform users. The list refreshes vendor health every 10 seconds.

The list shows:
- `Vendor`, `Protocol` (SMPP or HTTP);
- `Set up`: the number of binds, `profile set`, or `no profile`;
- `Routing`: `in routing`, `out: <reason>`, `paused`, `disabled` or `no data`;
- `Binds up`, `Sent / failed (30 s)`, `p95 submit`, `Failover` (on or off), `Added`.

`out:` reasons:
- `no bind up`;
- `slow submits`;
- `too many errors`;
- `low delivery rate`.

#### To add a vendor

Needs admin.

1. Click `New vendor`.
2. Enter a `Name`, an `ID` (2–32 characters of a–z, 0–9 and `-`; it names the vendor's queue and cannot be changed) and the `Protocol`.
3. Click `Create`. The vendor page opens. New vendors start **paused**.

#### The vendor page

**`Status` card:**
- `ID`, protocol, `Status` and `Routing`, with the reason and since when.
- One row per running connector (one per app server): binds up, sent and failed in the last 30 s, p95 submit time and rerouted messages. If none: `No connector is reporting. Connectors start a vendor within 30 seconds once it has an enabled bind or an HTTP profile and is not disabled.`
- Admins see `Activate`, `Pause` and `Disable`. Routers apply status changes within 15 seconds.

**`SMPP binds` tab** (SMPP vendors). Click `Add bind` and fill in:

| Field | Rule |
| --- | --- |
| `Host`, `Port` | Port 1–65535 |
| `TLS` | Yes or No |
| `System ID` | Up to 16 characters |
| `Password` | Up to 9 characters; stored encrypted, never shown again |
| `System type` | Optional |
| `SMPP version` | 3.3, 3.4 or 5.0 |
| `Bind type` | transceiver, transmitter or receiver |
| `Window` | 1–1000 |
| `TPS` | Positive |
| `PE ID TLV`, `Template ID TLV`, `TM chain TLV` | The tag the vendor wants for each, like `0x1400`. Ask the vendor |

The connectors apply a new or disabled bind within 30 seconds; the vendor reconnects. `Disable` turns a bind off. Every app server opens every enabled bind, so with three app servers a vendor that allows 6 binds needs 2 bind rows. Ask your operations team how many app servers there are.

**`HTTP profile` tab** (HTTP vendors):
- `Base URL`, `TPS`, `Concurrency` and `Secret`. Leave the secret empty to keep the stored one.
- Four JSON boxes: `Auth`, `Request`, `Response` and `Receipts`. Each box has a hint listing its keys.
- Click `Save profile`. The profile is checked the way the connector will load it, and errors are shown.
- Admins see the receipt path to give the vendor, with its token: the full URL is `https://<receipt host>/dlr/<vendor ID>?token=…`. Your operations team knows the receipt host (`DLR_HOST`).
- The connectors apply a saved profile within 30 seconds.

**`Failover` tab.** Takes the vendor out of routing when no bind is up, or when a limit is crossed. It brings it back after the cool-down once it is healthy. Queued messages move to the next route. 0 means not checked.

| Field | Default |
| --- | --- |
| `Failover` | On |
| `Max p95 submit (ms)` | 3,000 |
| `Max errors (%)` | 50 |
| `Min delivered (%)` | 0 (off) |
| `Receipt window (min)` | — |
| `Cool-down (s)` | 30 |

**`Costs` tab.** What the vendor charges us, by destination and category. Rows are `current`, `scheduled` or (with `show superseded`) `superseded`. Least-cost routing skips a vendor that has no cost for the destination. Import costs as CSV:
- `mcc` and `cost` are required;
- `mnc`, `category` and `valid_from` are optional.

---

### Routes

**Who:** platform users. Changes need admin.

How a route is chosen:
1. An account's own routes are tried before the default routes.
2. Among the routes matching the destination (its network, or the whole country with MNC `*`), lower priority numbers go first.
3. Routes with the same priority form a group:
   - `least cost` picks the cheapest healthy vendor that has a cost for the destination;
   - `priority` takes the route's vendor;
   - `weighted` splits traffic by weight.

Messages to a destination with no route are refused.

#### To add a route

1. Under `New route`, fill in:

   | Field | Meaning |
   | --- | --- |
   | `Account` | `Default (everyone)` or one account |
   | `MCC`, `MNC` | MNC `*` means the whole country |
   | `Mode` | `Least cost`, `Priority` or `Weighted` |
   | `Vendor` | For priority and weighted only |
   | `Priority` | Lower goes first; default 100 |
   | `Weight` | Weighted mode only |
   | `HLR` | `No lookup`, `Look up`, or `Look up, refuse invalid or absent` |

2. Click `Add route`. Routers apply it within 15 seconds.

To disable a route, click `Disable`. Tick `show disabled` to see disabled routes. The error `mode lcr (no vendor_id), or priority/weighted with a vendor_id` means the mode and vendor do not fit together.

---

### Firewall

**Who:** platform users. Changes need admin.

#### Block lists

Rules are checked at submit, after DLT. `Reject` refuses the message. `Flag only` just counts it. Changes apply within 30 seconds.

| `Kind` | Pattern | Refusal code |
| --- | --- | --- |
| `Sender` | Glob, any case, e.g. `*BANK` | `fw_sender` |
| `Content` | A phrase (whole words, any spacing, any case), or `re:<regexp>` | `fw_content` |
| `Link domain` | A domain; blocks it and its subdomains, e.g. `bit.ly` | `fw_url` |
| `Destination` | Glob on the number with country code, e.g. `9198100*` | `fw_destination` |

To add a rule:
1. Under `New rule`, choose the `Kind` and enter a `Pattern`.
2. Choose the `Action`, `Applies to` (`Every account` or one account), and add a `Note` if you like.
3. Click `Add rule`.

`Disable` turns a rule off. Use the `Kind` filter and `show disabled` to narrow the list.

#### Pumping guard

This protects against SMS pumping (OTPs triggered to number ranges on SIM farms) and floods. Choose the `Account`. The card shows `defaults` or `custom`.

| Field | Default | Refusal code |
| --- | --- | --- |
| `Messages to one number per hour` | 50 | `fw_destination_rate` |
| `Into one 1,000-number range per 10 min` | 300 | `fw_series` |
| `Block a tripped range for (min)` | 60 | `fw_series` |

0 turns a limit off. Click `Save`.

`Blocked ranges` lists the ranges blocked for this account, and for how many more minutes. Only that account is blocked from a range. If the traffic is genuine, click `Unblock`; this also resets the range's count. `Refresh` reloads the list.

---

### Vendor billing

**Who:** platform users. Imports and disputes need finance.

#### Invoices

The list shows `Invoice`, `Vendor`, `Period`, `Parts`, `Amount`, `Imported` and `Dispute` status. Filter by `Vendor`.

##### To import an invoice

1. Under `Import an invoice`, choose the `Vendor`, enter the `Invoice number`, and set `Period from` and `to` (both inclusive).
2. Upload the CSV with columns `day, mcc, mnc, segments, amount`:
   - one row per IST day and network;
   - days must be inside the period;
   - `mnc` may be `*` for country-level billing;
   - no duplicate day/mcc/mnc.
3. Click `Import`.

##### To reconcile and dispute

1. Click the invoice row. `Reconciliation` shows `Vendor billed`, `Our records`, `Disputable` and `Lines off`.
2. Adjust `Tolerance (%)` if needed (default 0.5, between 0 and 100). Untick `only lines that differ` to see every line.
3. Line statuses:

   | Status | Meaning |
   | --- | --- |
   | `ok` | Within tolerance |
   | `mismatch` | Parts or amount differ by more than the tolerance |
   | `not in our records` | The vendor billed it; we have no record |
   | `not on invoice` | We sent it; the vendor did not bill it |

4. The disputable amount is what the vendor billed above our figure on `mismatch` and `not in our records` lines. Underbilling is shown but never disputed.
5. If something is disputable, add `Notes for the vendor` and click `Dispute ₹…`.

Our records count messages the vendor accepted, at the vendor cost recorded per message. Each invoice can have only one open or sent dispute (`already exists`). If nothing is over tolerance, the error is `nothing to dispute: no overbilled lines outside tolerance`.

#### Disputes

The workflow is open → sent to the vendor → accepted or rejected → settled.

To move a dispute on:
1. Click its row.
2. Choose `Move to`. For `settled`, enter the `Settled amount (₹)`: the credit note received, up to the disputed amount (0 to write it off).
3. Add a `Note` if you like. It is added to the dispute's trail with the date and your e-mail.
4. Click `Update`.

A move that is not allowed gets `cannot move from … to …`.

---

### Number data

**Who:** platform users. Imports need admin.

A number's network is found from:
1. the MNP list (ported numbers);
2. then the number series;
3. then the country default.

When a route asks for an HLR lookup, the HLR answer is cached and wins over all of these.

**`Look up`:** enter a number (10 digits, or with country code). The page shows `Routed as`, `Number series` and `HLR (cached)`. Anything that is not an Indian mobile number gets `not an Indian mobile number`.

**`Number series`:**
- Search by prefix, operator or circle. The first 500 matches are shown.
- Import CSV `prefix, mcc, mnc[, operator, circle]`. Prefixes are national, 2–9 digits; the longest matching prefix wins.
- Tick `replace the whole table` to replace everything. Otherwise rows are added or updated.
- A bad line stops the import. Edges reload series within 5 minutes.

**`Ported numbers (MNP)`:**
- Shows the import history.
- Import CSV `msisdn, mcc, mnc`. An empty `mnc` removes a number that ported back.
- Bad lines are skipped and listed. Changes take effect at once.
- For large files, use the API.

---

### Error codes

**Who:** platform users. Imports need admin.

Vendors put their own error codes in delivery receipts. This page maps each vendor code to one reason and a 3-digit code the customer sees, so receipts read the same whichever vendor carried the message. A mapping for one vendor wins over the one for all vendors (`*`).

Filter by `Vendor` or search with `Find`.

To import mappings:
1. Under `Import mappings`, choose `For vendor` (or `All vendors (*)`).
2. Upload CSV `code, reason, customer_code[, description]`:
   - `reason`: a–z, 0–9 and `_`;
   - `customer_code`: 3 digits.
3. Click `Import`.

Existing codes are updated. A bad line stops the import. The receipt processor reloads within a minute. Codes with no mapping pass through as they are.

---

## 3. Common workflows

### Onboarding a new direct customer

Done by a platform admin; the payment step needs finance.

1. **Create the account.** `Accounts` → `New account`. Set `Name`, `Type` `Customer` and a platform `Rate card`, then `Create`.
2. **Add funds.** Click `Wallet` on the account. Under `Record a payment (top-up)`, enter the amount and the payment reference, then `Add funds`.
3. **Credit (optional).** Under `Credit limit`, set the limit and a reason. Leave it at 0 for prepaid.
4. **People.** On the account's `People` tab, add the customer's admin with role `admin` (or `owner` if you are an owner). Make sure they have a verified Keycloak login with the same e-mail.
5. **Connections.** On the `API & SMPP` tab:
   - for SMPP, create SMPP credentials, then pass the system ID and password to the customer;
   - for HTTP, the customer's admin can create their own API keys and webhook after signing in. You can also do it here.
6. **DLT.** `DLT` → `Account`: the customer. Then:
   1. `Add entity` with the customer's PE ID and TM chain.
   2. `Import` headers, then templates, from the customer's DLT portal export.
   3. Use `Check a message` with a real text to confirm it would be accepted.
7. **Special prices (optional).** `Deals` → `Account`: the customer → `Grant a deal`.
8. **Alerts.** Ask the customer to set a low-balance alert on their `Alerts` page. The portal sets alerts only for your own account.

### Onboarding a reseller with its own brand and domain

1. **Platform: create the reseller.** `Accounts` → `New account` with `Type` `Reseller` and a platform `Rate card`. This is the price the reseller pays the platform.
2. **Platform: fund it.** Top up its wallet, and set a credit limit if agreed.
3. **Platform: add its first person** on the `People` tab.
4. **Reseller: brand.** `Branding` → set `Display name`, `Logo`, `Colour` and `Support e-mail` → `Save`.
5. **Reseller: prices.** `Rate cards` → `New rate card`, then set prices or import them. These are what its sub-accounts pay. The reseller cannot create sub-accounts until it has a card.
6. **Domain (optional).** In this order:
   1. The reseller points a CNAME for its domain at the portal host.
   2. The platform adds a TLS certificate for the domain on the load balancer.
   3. The platform adds `https://<domain>/callback/` as a redirect URI, and `https://<domain>/login/` as a post-logout redirect URI, to the `amaze-portal` client in Keycloak.
   4. The platform opens `Branding`, chooses the reseller in `Brand of`, enters the `Portal domain` and clicks `Save`.

   The sign-in page on that domain now carries the reseller's brand.
7. **Reseller: sub-accounts.** `Sub-accounts` → `New account`. Then `Wallet` → `Transfer from your wallet`, and `Credit limit` if needed. Then SMPP credentials or API keys, and DLT registrations, as for a direct customer.

### Adding a vendor and routing traffic to it

All steps are on the platform.

1. **Create it.** `Vendors` → `New vendor` (name, ID, protocol). It starts paused.
2. **Connect it.**
   - SMPP: `SMPP binds` → `Add bind`, with the host, credentials, version, bind type, window, TPS and the vendor's DLT TLV tags.
   - HTTP: `HTTP profile`: fill it in, `Save profile`, and give the vendor the receipt URL shown.
3. **Costs.** `Costs` → `Import costs`. Least-cost routing ignores a vendor with no cost for a destination.
4. **Error codes.** Get the vendor's receipt error codes, including its DLT codes. Import them on `Error codes` for this vendor.
5. **Wait for the connectors.** Within 30 seconds of step 2, the connectors start the vendor on their own; nothing needs to be done on the servers. The `Status` card then shows a connector row per app server and, for SMPP, binds up. A disabled vendor is not started.
6. **Failover.** Review the `Failover` tab and save if you change anything.
7. **Activate.** Click `Activate`.
8. **Routes.**
   - If a default least-cost route already covers the destination, the vendor competes on cost.
   - To send traffic to it directly, add a `Priority` or `Weighted` route on `Routes`, for everyone or for one account.
9. **Watch it.** On `Live monitor`, check the vendor shows `up`, with binds and submits. On the vendor page, check sent and failed counts and p95.
10. **Commitment (optional).** If the vendor sold committed volume, add it under `Deals` → `Vendor volume commitments`.

### Investigating an undelivered message

1. **Find the message.** Someone on the sender's account opens `Messages` and searches by mobile number, reference or message ID. Platform or reseller staff can open a known message ID with `/messages/view/?id=<message ID>&account=<account ID>`.
2. **Not found at all?** It was probably refused at submit. Refusals are not stored. The sender got the reason in the HTTP 422 response or SMPP status. Reproduce DLT refusals with `DLT` → `Check a message`. See refusals by reason on `Live monitor`.
3. **Read the status and error** on the message detail (see below).
4. **Check by cause:**

   | Error shows | Look at |
   | --- | --- |
   | `dlt_…` | `DLT`: is the header registered and active? Does a template match? Is the TM chain set? Use `Check a message` |
   | `fw_…` | `Firewall`: block lists, the account's limits, `Blocked ranges` |
   | `HLR_INVALID`, `HLR_ABSENT` | The route looks the number up and refuses invalid or absent numbers. `Number data` → `Look up` shows the cached HLR answer |
   | Vendor code without our suffix | The code is not mapped: add it on `Error codes` |
   | `expired` with no error | No receipt came back. Check the vendor on `Vendors` (routing state, connector) |

5. **Network.** Check `Network (MCC/MNC)`. If it looks wrong, `Number data` → `Look up` shows how the number resolves (MNP, series, default).
6. **Vendor.** Check `Vendor` and `Vendor message ID`. Quote the vendor message ID when you ask the vendor.
7. **Charged wrongly?** The account can raise a dispute with the message IDs (see [Disputes](#disputes)).

#### Reading an error code

An error code keeps the vendor's raw code and adds ours after `|`. For example, `UNDELIV:1703|dlt_template:603` means:
- the vendor reported `UNDELIV:1703`;
- we mapped it to the reason `dlt_template`;
- the customer's receipt carries `err:603`.

Operator DLT receipt codes:

| Code | Reason |
| --- | --- |
| 600 | `dlt_other` |
| 601 | `dlt_entity` |
| 602 | `dlt_header` |
| 603 | `dlt_template` |
| 604 | `dlt_category` |
| 605 | `dlt_promo_hours` |
| 606 | `dlt_consent` |
| 607 | `dlt_tm_chain` |
| 608 | `dlt_url` |
| 609 | `dlt_callback` |
| 610 | `dlt_dnd` (the operator's do-not-disturb list) |

### Month-end statements

**For each account** (account's own finance staff, or its manager):
1. Open `Reports` → `Wallet statement`.
2. Choose the `Account`. Set `From` to the 1st of the month and `To (exclusive)` to the 1st of the next month.
3. Click `Show`. Check the opening and closing balances and the totals per kind.
4. Click `Download CSV` for every ledger entry.
5. Use the `Usage` tab for messages, parts, delivery and charges per day.

Pick the `Time zone` first (see [Time zone](#time-zone)): the period runs from midnight on the first day to midnight on the `To` day, in that zone.

**Resellers:** the `Usage` tab on your own account shows `Via sub-accounts`: what your sub-accounts sent that was charged to you. Run statements for each sub-account to see what you charged them.

**Platform, vendor side:**
1. `Vendor billing` → `Import an invoice` for each vendor invoice.
2. Review the reconciliation, and dispute overbilling.
3. Move disputes along as the vendor answers, and record the settled amount when the credit note arrives.
4. Check `Deals` → `Vendor volume commitments` for any projected shortfall.

---

## 4. Troubleshooting and FAQ

### Messages you may see

| Message | What it means | What to do |
| --- | --- | --- |
| `signed in, but no platform user for this email` | Keycloak signed you in, but the portal has no active user for this e-mail, or your account is suspended | Ask an admin of your account (or your provider) to add you on `Team`, or to reactivate the account |
| `Your session ended. Please sign in again.` | Your sign-in expired | Sign in again |
| `This page is not available for your account.` | Your account's role cannot use this page | — |
| `your role (<role>) does not allow this` | Your user role is too low | Ask an admin to change your role |
| `not allowed for this account` | The account is not yours and you do not manage it | Pick another account |
| `already exists` | A duplicate: an e-mail used elsewhere, a domain taken, an open vendor dispute for this invoice | Use a different value |
| `invalid JSON: …` | The request could not be read, for example a request body over 64 KB | Make it smaller (see the logo note under [Branding](#branding)) |
| `… needs CLICKHOUSE_URL`, `usage store unavailable`, `message store unavailable` | The message store is not configured or not reachable | Platform operations: check ClickHouse |
| `needs Redis`, `firewall store unavailable`, `wallet cache unavailable` | Redis is not reachable | Platform operations: check Redis |
| `live monitoring needs PROMETHEUS_URL`, `monitoring store unavailable` | Prometheus is not configured or not reachable | Platform operations: check Prometheus |
| `SECRETS_KEY is not configured` | Vendor passwords and secrets cannot be encrypted | Platform operations: set the key |
| `Files over 20 MB: use the API` | The portal uploads CSV files up to 20 MB | Split the file, or use the admin API |

### FAQ

**A customer's messages are all refused. Where do I start?**
1. Check the account is `active`.
2. Check `Available to send` is above zero.
3. Check the account has a rate card.
4. Run `DLT` → `Check a message` with one of their texts.
5. Look at `Live monitor` → `Refusals, last 5 minutes` for the reason.

**We topped up but the customer still cannot send.**
- Check `Available to send` on `Wallet`. If the account was using credit, the top-up first repays the credit used.
- If the account is suspended, top-ups do not help: reactivate it.

**Why does the balance differ from the settled balance?**
Charges are written to the ledger in batches. `Balance` is live. `settled` catches up within moments.

**Can a reseller set its own domain?**
No. Only the platform sets portal domains, after adding the TLS certificate and the sign-in redirect. See [Onboarding a reseller](#onboarding-a-reseller-with-its-own-brand-and-domain).

**A reseller cannot reactivate its sub-account.**
The platform suspended it. Only the platform can lift that suspension.

**How fast do changes take effect?**

| Change | Takes effect |
| --- | --- |
| Prices, rate card changes, deals, DLT registrations, suspensions | Within 1 minute |
| API key revocation | Within 1 minute |
| Routes, vendor status, failover policy, vendor commitments | Within 15 seconds |
| Firewall rules | Within 30 seconds |
| Number series | Within 5 minutes |
| MNP imports | At once |
| SMPP vendor binds and HTTP profiles | Within 30 seconds (the vendor reconnects) |
| New vendors, and disabling a vendor | Within 30 seconds (the connectors start or stop it) |

**Why is the message I am looking for not in `Messages`?** Either:
- it was refused at submit (refusals are not stored);
- it belongs to another account (the page searches only your own);
- the date range missed it: dates are midnight in the chosen `Time zone`, and the `To` day is not included (see [Time zone](#time-zone)).

**Can I delete an account, a rate card price, a route or a firewall rule?**
- Routes, firewall rules, API keys, SMPP credentials, vendor binds and people can be **disabled**, revoked or removed. They stay listed as disabled.
- Accounts can be suspended.
- Prices are changed by setting a new one.

### Trying it locally

A developer setup (`scripts/dev-local.sh up`, see the README) runs the portal at `http://localhost:8081`. Instead of single sign-on, it shows a list of users to pick from. The seeded users are:

| User | Account |
| --- | --- |
| `ops@amaze.local` | The platform |
| `owner@demo-reseller.local` | A reseller, brand `BlueText SMS` |
| `owner@reseller-shop.local` | That reseller's sub-account |
| `owner@demo-customer.local` | A direct customer |

This works only on development machines (`APP_ENV=dev`).

---

## 5. Glossary

| Term | Meaning |
| --- | --- |
| **A2P** | Application-to-person: messages sent by a business system to people |
| **Available** | Balance + credit limit − in flight. What an account can still spend |
| **Bind** | An SMPP connection. A *transmitter* sends, a *receiver* receives, a *transceiver* does both |
| **Category (DLT)** | Transactional, service implicit, service explicit or promotional. It decides which headers a template may use and, for promotional, the sending hours (09:00–21:00 IST) |
| **CNAME** | A DNS record that points one domain name at another; used for reseller portal domains |
| **Consent template** | A DLT registration a service explicit template must be linked to |
| **Credit limit** | How far below zero a wallet may go. 0 means prepaid |
| **DLR** | Delivery receipt (delivery report): the vendor's message saying whether a message was delivered |
| **DLT** | Distributed Ledger Technology: India's registry of senders, headers and message templates, run by the operators. Messages that do not match it are blocked |
| **Failover** | Taking a vendor out of routing automatically when it is down, slow, failing or delivering badly |
| **Header** | The sender ID shown on the phone. 6 letters, or 6 digits for promotional traffic. Registered per PE ID |
| **HLR** | Home Location Register lookup: asks the network whether a number is valid and which network serves it |
| **Idempotency key** | A request ID that stops the same payment or transfer from being booked twice |
| **In flight** | Money held for messages accepted but not yet charged |
| **LCR** | Least-cost routing: picking the cheapest healthy vendor for a destination |
| **MCC / MNC** | Mobile country code and mobile network code. India is MCC 404 and 405. MNC `*` means every network |
| **MNP** | Mobile number portability: a number moved to another operator |
| **Part (segment)** | One SMS. A long message is sent as several parts and charged per part |
| **PE ID** | Principal Entity ID: the business's registration number on DLT |
| **Rate card** | A price list per destination and DLT category |
| **Reseller** | An account that sells to its own sub-accounts under its own brand |
| **SMPP** | Short Message Peer-to-Peer: the standard protocol for sending SMS between systems |
| **system_id** | The SMPP user name |
| **Template** | A registered message text with placeholders such as `{#var#}`. Every message must match one of its header's templates |
| **TLV** | An optional SMPP field (tag, length, value). DLT fields travel in TLVs whose tags vendors choose |
| **TM chain** | The telemarketer chain: the registered path from the PE through the telemarketers that carry its messages. Sent with every message |
| **TPS** | Messages per second |
| **UDH** | User data header: marks the parts of a long message that the sender split itself |
| **Webhook** | A URL the platform calls with an HTTPS POST, here for delivery receipts and low-balance alerts |
