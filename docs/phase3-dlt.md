# Phase 3 slice 1: India DLT enforcement

Every message is checked against the sender's DLT registrations at submit, before anything is priced or held. A message that operator scrubbing would reject is refused immediately, with a reason. Slice 2 covers the firewall, HLR/MNP and vendor failover.

## What is checked

The checks run in this order; the first failure is the reason returned.

| # | Check | Reason |
| --- | --- | --- |
| 1 | The PE ID, if given, is registered to this account and active | `dlt_entity` |
| 2 | The sender is a registered, active header (under that PE ID if given; otherwise its own PE ID is used) | `dlt_header` |
| 3 | Template ID given: it is registered for this header, active, and the text matches it. No template ID: the text matches one of the header's active templates, and the most specific match wins | `dlt_template` |
| 4 | Promotional templates only on promotional (numeric) headers, and the others only on alphabetic headers | `dlt_category` |
| 5 | Promotional messages only 09:00–21:00 IST | `dlt_promo_hours` |
| 6 | Service explicit templates have a consent template | `dlt_consent` |
| 7 | Every link in the variable parts is whitelisted on the template (APK links as APKs), and every `{#cbn#}` value is a whitelisted callback number | `dlt_url`, `dlt_callback` |
| 8 | A TM chain: the customer's own, else the one registered for the PE ID. Refused if neither exists and `DLT_REQUIRE_TM_CHAIN=true` (the default) | `dlt_tm_chain` |

The accepted message carries the PE ID, matched template ID, category and TM chain to the vendor (TLVs or HTTP parameters), into receipts and into ClickHouse (`pe_id`, `template_id`, `dlt_category`).

### Template matching

- **Literal text** must match exactly, including case. Any run of spaces or line breaks matches any other run.
- **Placeholders:** placeholder names are case-insensitive. An unknown placeholder name is treated as `{#var#}`.

| Placeholder | Matches |
| --- | --- |
| `{#var#}` | anything, 0–30 characters (TRAI limit) |
| `{#numeric#}` | digits, `.` and `,`, up to 30 |
| `{#alphanumeric#}` | letters and digits, up to 30 |
| `{#url#}`, `{#urlott#}` | one link, up to 100 characters; must be whitelisted |
| `{#cbn#}` | a phone number; must be a whitelisted callback |
| `{#email#}` | an e-mail address |

- **Links found anywhere in a variable** (with a scheme, `www.`, or a bare domain on a common TLD) must be whitelisted.
  - **Same host only:** a whitelisted `amz.in` covers `amz.in/anything`, but not `pay.amz.in` or `amz.in.evil.com`.
  - **Path entries:** a whitelisted `pay.example.com/checkout` covers `/checkout` and `/checkout/42`, but not `/checkoutfake`.
- **Pre-split messages:** a message the customer split itself (UDH parts) can't be matched against a template part by part. It must carry a template ID, which is checked against the header without matching text.
- **Speed:** each edge caches an account's registrations for 1 minute, with templates compiled. A message is first tested for the template's literal words in order, which is cheap, and only candidates that pass run the full pattern.
  - Auto-detecting among 500 near-identical templates on one header takes 57 µs per message (`BenchmarkAutoDetect`); the usual 5–50 templates take a few µs.
  - A message that names its template ID is matched against that template alone.

### Turning it off

`PUT /v1/accounts/{id}/dlt-mode {"mode":"off","reason":"..."}` (platform only) skips the checks for one account, for example a test account. Every change is audited.

## Refusal codes

| Reason | HTTP (422 `code`) | SMPP `command_status` | Receipt `err:` (operator failures) |
| --- | --- | --- | --- |
| `dlt_entity` | dlt_entity | 0x410 | 601 |
| `dlt_header` | dlt_header | 0x411 | 602 |
| `dlt_template` | dlt_template | 0x412 | 603 |
| `dlt_category` | dlt_category | 0x413 | 604 |
| `dlt_promo_hours` | dlt_promo_hours | 0x414 | 605 |
| `dlt_consent` | dlt_consent | 0x415 | 606 |
| `dlt_tm_chain` | dlt_tm_chain | 0x416 | 607 |
| `dlt_url` | dlt_url | 0x417 | 608 |
| `dlt_callback` | dlt_callback | 0x418 | 609 |
| `dlt_dnd` | — (operator only) | — | 610 |
| `dlt_other` | — (operator only) | 0x41F | 600 |

The HTTP `error` field says exactly what is missing, for example `header ABCDEF is not registered` or `text does not match template 1107…`. Refusals are counted in `sms_refused_total{reason}`.

## Operator scrubbing failures

Operators still scrub, for example against DND/NCPR for promotional traffic. Vendors report a failure as a receipt error code, and each vendor uses its own codes.
- **Mapping:** `vendor_error_codes` maps a vendor's code (or a code for every vendor) to a reason and the code the customer sees.
- **Error code format:** the message's `error_code` keeps the vendor's raw code for support and adds ours, e.g. `UNDELIV:1703|dlt_template:603`.
- **What the customer sees:** the receipt carries `err:603`, and the webhook and status API carry the full value.

Import with `POST /v1/vendor-error-codes/import[?vendor=<id>]` (CSV `code,reason,customer_code[,description]`). The DLR processor reloads within a minute. Before going live, get each vendor's DLT error code list and import it. Unmapped codes pass through as before.

## Registrations (admin API)

An account manages its own DLT data, and so do its managers (the platform, or its reseller). Imports are all or nothing: a file with any bad line imports nothing and lists every bad line.

| Method and path | What |
| --- | --- |
| `GET /v1/accounts/{id}/dlt` | Mode, PE IDs with header/template counts and TM chain |
| `POST /v1/accounts/{id}/dlt/entities` | Register or update a PE ID (`pe_id`, `name`, `status`, optional `tm_chain`); a PE ID belongs to one account |
| `PUT /v1/accounts/{id}/dlt/entities/{pe}/chain` | Set the PE–TM chain value (the previous one is closed, kept for history) |
| `POST …/entities/{pe}/headers/import` | CSV `header,category[,status]`; headers must be 6 letters (6 digits if promotional); category `Others` counts as service implicit |
| `POST …/entities/{pe}/templates/import` | CSV of templates, see below; replaces each listed template's headers and whitelist |
| `GET …/entities/{pe}/templates[?header=]` | Templates with headers and whitelist |
| `POST /v1/accounts/{id}/dlt/check` | Dry run: `sender`, `text`, optional `pe_id`, `template_id`, `tm_chain`, `at`; returns the template it would match, or the reason with its codes |
| `GET /v1/accounts/{id}/dlt/report?by=template\|header\|pe_id\|category&from=&to=` | From ClickHouse: messages, segments, delivered, not delivered, operator DLT failures and the commonest DLT reason per key |
| `PUT /v1/accounts/{id}/dlt-mode` | Platform only |
| `POST /v1/vendor-error-codes/import`, `GET /v1/vendor-error-codes` | Platform only |

**Template CSV.** Columns are found by name, not position, so operator portal exports can be uploaded as they are (save Excel files as CSV).

| Column | Names accepted |
| --- | --- |
| template ID | Template ID, Content Template ID, DLT Template ID |
| category | Category, Template Type, Content Type: Transactional, Service Implicit, Service Explicit, Promotional |
| header(s) | Header, Headers, Sender ID; several separated by `;` |
| body | Body, Template Content, Content, Message |
| optional | Template Name, Consent Template ID (required for service explicit), Status (Active, or Deactivated/Inactive/Blocked → suspended), URLs, APKs, Callbacks (`;` separated) |

## Not in this slice

- **Messages refused at submit are not in ClickHouse.** The DLT report covers operator failures; submit refusals are visible through the metric.
- **No DND/NCPR list of our own.** Operators scrub promotional traffic against it, and their failures come back as `dlt_dnd` once the codes are mapped.
- **Consent is not verified per recipient.** We check that a service explicit template has a consent template; recipient consent records live with the operators.
- **No operator portal sync.** Data comes in by CSV import.

## Verification

- **Unit tests:**
  - template matching: placeholders, whitespace, case, the 30-character limit, Hindi text, back-to-back placeholders;
  - whitelist: hosts, paths, subdomain tricks, APKs, callbacks, "Rs.500" not taken for a link;
  - every check and refusal reason, UDH parts, promotional hours, mode off;
  - header and category rules.
- **Admin integration test** (Postgres):
  - portal-style CSV import, and all-or-nothing on a bad line;
  - promotional template on a transactional header refused, and service explicit without consent refused;
  - dry-run check through chain → OK, auto-detect, link whitelist and promotional hours;
  - suspending a template by re-import;
  - access rules, mode off, and vendor error code import and lookup.
- **End to end on the local stack:**
  - HTTP: an auto-detected template was accepted, with PE ID, template ID and category stored. Unregistered header, no matching template, the wrong template ID, a foreign PE ID and a 31-character variable were each refused with the right reason.
  - SMPP: 100 non-matching messages were all refused with 0x412.
  - Scrubbing: sim's `UNDELIV err 001` was mapped to `dlt_template/603`, and all 12 of 12 failures carried `UNDELIV:001|dlt_template:603`.
  - The DLT report attributed the failures to the right template and category.
- **Load with DLT enforced** (one 4-vCPU box, template ID given):
  - 1,500 TPS: 18 / 63 / 102 ms (p50 / p95 / p99), against 17 / 52 / 84 ms before DLT.
  - 2,500 TPS: 2,493 accepted, 0 failures, all receipts back.
  - `billing-check` passed.
