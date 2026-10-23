# Amaze A2P SMS: API reference

This reference is for customers and resellers sending SMS to Indian mobile numbers through the platform, over HTTP REST or SMPP. Everything here describes what the platform does today, taken from the code (`internal/edge/httpapi`, `internal/ingest`, `internal/dlt`, `internal/edge/smppserver`, `internal/smpp`, `internal/webhook`, `internal/dlr`). Hostnames are placeholders: ask your provider for the real ones.

Contents

1. [Overview](#1-overview)
2. [Authentication, limits and idempotency](#2-authentication-limits-and-idempotency)
3. [HTTP API](#3-http-api)
4. [Delivery receipts for HTTP customers](#4-delivery-receipts-for-http-customers)
5. [SMPP](#5-smpp)
6. [Error codes and message statuses](#6-error-codes-and-message-statuses)
7. [Quick start and FAQ](#7-quick-start-and-faq)

---

## 1. Overview

### 1.1 Interfaces and addresses

| Interface | Address (placeholder) | Notes |
| --- | --- | --- |
| HTTP REST | `https://<API_HOST>`, e.g. `https://api.example.com` | JSON over HTTPS (port 443). Paths start with `/v1`. Plain `http://` is redirected to HTTPS. |
| SMPP, plain | `smpp.example.com:2775` | SMPP 3.3, 3.4 and 5.0 |
| SMPP over TLS | `smpp.example.com:3550` | Same service; TLS is terminated at the load balancer. TLS 1.2 or later when the server terminates TLS itself. |
| Customer portal | `https://portal.example.com`, or your reseller's own domain | Create API keys, set the webhook, manage DLT registrations, search messages. Its API is not for programmatic use (see [2.1](#21-api-keys)). |
| Receipts from HTTP vendors | `https://<DLR_HOST>/dlr/<vendor>?token=…`, e.g. `https://dlr.example.com/dlr/vendor-a?token=…` | Not for customers: only for vendors that carry the platform's traffic over HTTP. The platform's operations team gives each vendor its URL and token. Other paths on this host answer `404`. |

`API_HOST` and `DLR_HOST` are the names your provider set up; the examples below use `api.example.com` for `API_HOST`.

Both interfaces go through the same acceptance pipeline: the same number and sender checks, DLT checks, firewall, pricing and balance hold. A message is acknowledged (HTTP `202`, or `submit_sm_resp` with status 0) only after it is durably queued and its cost is held against your balance.

### 1.2 Destinations

Launch scope is India only. Every destination must be an Indian mobile number. These forms are accepted (spaces, `-`, `(` and `)` are ignored):

| Form | Example |
| --- | --- |
| `+91` and 10 digits | `+91 98123 45678` |
| `91` and 10 digits | `919812345678` |
| `0091` and 10 digits | `0091-9812345678` |
| `0` and 10 digits | `09812345678` |
| 10 digits | `9812345678` |

The 10-digit national number must start with 6, 7, 8 or 9. Anything else is refused (`invalid_destination`, SMPP `0x0B`). The platform stores and returns the number as `91XXXXXXXXXX` (no `+`).

### 1.3 Sender (header)

The sender must be 1 to 11 letters and digits, or 3 to 15 digits. Under DLT it must also be one of your registered headers: 6 letters for transactional and service headers, 6 digits for promotional headers. Header matching is case-insensitive.

### 1.4 DLT requirements (TRAI TCCCPR)

Every message is checked against your DLT registrations when you submit it, before anything is charged. A message that operator scrubbing would reject is refused immediately with a reason. Your registrations (PE ID, headers, templates, TM chain) are entered in the portal's DLT page by you or your provider (CSV import of operator-portal exports is supported). Changes reach the sending edges within 1 minute.

The checks run in this order. The first one that fails is the reason returned.

| # | Check | Reason |
| --- | --- | --- |
| 1 | If you send a PE ID, it is registered to your account and active. | `dlt_entity` |
| 2 | The sender is a registered, active header: under the PE ID you sent, or, if you sent none, under whichever of your PE IDs owns it (that PE ID is then used). | `dlt_header` |
| 3 | If you send a template ID, it is registered for this header, active, and the text matches it. If you do not, the text must match one of the header's active templates; the most specific match (most literal text) wins. | `dlt_template` |
| 4 | Promotional templates only on promotional (numeric) headers; other categories only on alphabetic headers. | `dlt_category` |
| 5 | Promotional messages only between 09:00 and 21:00 IST. | `dlt_promo_hours` |
| 6 | A service explicit template must have a consent template. | `dlt_consent` |
| 7 | Every link in the variable parts is whitelisted on the template (APK links as APKs), and every `{#cbn#}` value is a whitelisted callback number. | `dlt_url`, `dlt_callback` |
| 8 | A telemarketer (PE-TM) chain: the one you send, else the one registered for the PE ID. Refused when neither exists (platform default). | `dlt_tm_chain` |

So the PE ID, template ID and TM chain are all optional on the wire as long as your registrations let the platform find them. The accepted message carries the resolved PE ID, template ID, category and TM chain to the operator.

Your provider can turn DLT checks off for one account (for example a test account). Then none of the checks above run.

#### Template matching rules

- Literal text must match exactly, including case. Any run of spaces, tabs or line breaks matches any other run. Leading and trailing whitespace of the message is ignored.
- Placeholder names are case-insensitive. An unknown placeholder name is treated as `{#var#}`.

| Placeholder | Matches |
| --- | --- |
| `{#var#}` | any characters, 0 to 30 |
| `{#numeric#}` | digits, `.` and `,`, 0 to 30 |
| `{#alphanumeric#}` | letters and digits, 0 to 30 |
| `{#url#}`, `{#urlott#}` | one run without spaces, 1 to 100 characters; must be a whitelisted link |
| `{#cbn#}` | a phone number (digits, `+`, spaces, `-`); must be a whitelisted callback number |
| `{#email#}` | an e-mail address |

- Links found anywhere inside a variable value (with `http://` or `https://`, starting `www.`, or a bare domain on a common TLD such as `.com`, `.in`, `.ly`) must be whitelisted on the template.
  - Same host only: a whitelisted `amz.in` covers `amz.in/anything`, but not `pay.amz.in`.
  - A whitelisted path covers that path and anything below it: `pay.example.com/checkout` covers `/checkout` and `/checkout/42`, not `/checkoutfake`.
- Messages you split yourself (SMPP UDH parts) cannot be matched whole. Each part must carry a template ID, and the part's text must be a possible contiguous piece of a message matching that template.

### 1.5 Encoding and segments

The platform chooses the encoding from the text:

| Encoding | When | Single SMS | Each part of a long SMS |
| --- | --- | --- | --- |
| GSM 03.38 (7-bit) | every character is in the GSM default alphabet or its extension table | 160 characters | 153 characters |
| UCS-2 | any other character (Hindi and other Indic scripts, emoji, curly quotes, etc.) | 70 characters | 67 characters |

- GSM extension characters count as 2: `^ { } \ [ ~ ] | €` and form feed.
- In UCS-2, characters outside the Basic Multilingual Plane (most emoji) count as 2 (UTF-16 code units).
- A message may be at most **10 segments**. Longer text is refused (`invalid_text`, SMPP `0x01`).
- When the platform splits a long message, it never splits a GSM escape pair or a UTF-16 surrogate pair.

Examples: 160 GSM characters is 1 segment; 161 is 2; 306 is 2; 307 is 3. 70 Hindi characters is 1 segment; 71 is 2.

### 1.6 Billing

- Price is per segment and depends on the destination network (resolved from MNP data and number series). It comes from your rate card or, if you have one, a customer deal. `cost` = unit price x segments, in INR, held when the message is accepted.
- Your account can send while `available = balance + credit_limit - held` covers the cost. The credit limit is 0 unless your provider raises it.
- If you are a reseller's sub-account, the reseller's wallet is also held at the reseller's own price. If it cannot cover the message you get `reseller_billing` (HTTP `402`, SMPP `0x404`).
- When the hold becomes a charge:

| Outcome | Charged? |
| --- | --- |
| The vendor accepted the message (status `submitted`), whatever the final receipt says (`delivered`, `failed`, `expired`) | Yes |
| Refused by the platform after acceptance (status `rejected`: no route, HLR invalid/absent) | No, hold released |
| Refused by the vendor before any part was accepted (status `failed`, no vendor message ID) | No, hold released |
| Refused at submit (any HTTP 4xx, any non-zero `submit_sm_resp` status) | Nothing was held |

---

## 2. Authentication, limits and idempotency

### 2.1 API keys

HTTP requests are authenticated with an API key. Keys look like:

```
amz_<12 hex characters>_<48 hex characters>
```

Send the key in either header:

```
X-API-Key: amz_3f9a1c0b2d4e_6c1f...e0
Authorization: Bearer amz_3f9a1c0b2d4e_6c1f...e0
```

`X-API-Key` is checked first.

| Topic | Behaviour |
| --- | --- |
| Create | Portal, **Settings > API & SMPP > API keys**, by a user with the admin or owner role on the account, or by your reseller/provider for you. The full key is shown **once**; only a hash is stored. |
| List | Same page. Shows each key's prefix, name, created and revoked times. |
| Revoke | Same page. A revoked key is refused within **60 seconds** (verified keys are cached for up to a minute on each node). |
| Suspended account | Its keys are refused (`401`) within 60 seconds. |
| Invalid or missing key | `401 {"code":"unauthorized","error":"invalid or missing API key"}` |

The admin API behind the portal (`/v1/accounts/...` on the portal host) takes only portal sign-in tokens (OIDC), never API keys. It is for the portal, not for integrations.

### 2.2 SMPP credentials

SMPP accounts (`system_id` and password) are issued by your provider (the platform or your reseller):

| Setting | Limit / default |
| --- | --- |
| `system_id` | 1 to 15 characters |
| `password` | 6 to 8 characters |
| `tps` | per bind; default 100 |
| `max_binds` | across all SMPP nodes; default 4 |
| `allowed_ips` | optional list of IPs or CIDRs; empty allows any address |

A disabled credential refuses new binds; binds already open stay up until they reconnect.

### 2.3 Rate limits

| Interface | Limit | Burst | When exceeded |
| --- | --- | --- | --- |
| HTTP `POST /v1/sms` | per account, per API node: 200 requests/s by default (deployment setting `HTTP_ACCOUNT_TPS`) | 1/5 of the rate (40 by default) | `429 {"code":"throttled"}` |
| SMPP `submit_sm` | per bind: the credential's `tps` | 1/10 of the rate (10 at 100 TPS) | `submit_sm_resp` `0x00000058` (ESME_RTHROTTLED) |

- `GET` endpoints are not rate limited.
- The HTTP limit is kept separately by each API node behind the load balancer, so the effective account limit scales with the number of nodes. Treat the configured value as your contracted rate.
- SMPP throughput for a `system_id` is `tps` x the number of transmitter/transceiver binds.
- Back off and retry throttled messages; nothing was held for them.

Separately, the firewall limits messages per destination number and per 1,000-number range to stop SMS pumping (see `fw_destination_rate` and `fw_series` in [section 6](#6-error-codes-and-message-statuses)). Defaults: 50 messages to one number per hour; 300 messages into one 1,000-number range per 10 minutes, after which that range is blocked for your account for 60 minutes. Your provider can change these per account.

### 2.4 Idempotency (HTTP)

Send an `Idempotency-Key` header (at most 64 characters) on `POST /v1/sms` to make retries safe.

- Keys are scoped to your account and remembered for **96 hours**.
- A repeat with a key already used returns `202` with `"duplicate": true` and the **original** message `id`. Nothing is sent or charged again. `segments` and `cost` come from the original message's record; they read `0` / `"0.0000"` if that record has expired or the first request is still in progress.
- The key is only claimed after the request passes validation, DLT and firewall checks. If the request is refused, or fails with `503`, the key is released, so a retry with the same key is processed normally.
- The request body is not compared. Reusing a key with a different body returns the first message.
- Do not send two requests with the same key at the same time: the second may get `duplicate: true` before the first has finished, and if the first then fails, the second has an `id` that does not exist.

SMPP has no idempotency key. Each `submit_sm` answered with status 0 is one message.

---

## 3. HTTP API

### 3.1 Conventions

- Request bodies are JSON, at most 64 KiB. Unknown fields are refused (`400 invalid_json`).
- Responses are JSON with `Content-Type: application/json`.
- Amounts are decimal strings in INR.
- Times are RFC 3339 in UTC with fractional seconds, e.g. `2026-10-23T09:15:02.123Z`.
- Every error has this shape:

```json
{"code": "dlt_template", "error": "dlt_template: text does not match template 1107160000000012345"}
```

`code` is stable and meant for programs. `error` is for people and may change.

### 3.2 `POST /v1/sms`: send one SMS

**Headers**

| Header | Required | Notes |
| --- | --- | --- |
| `X-API-Key` or `Authorization: Bearer` | yes | [2.1](#21-api-keys) |
| `Content-Type: application/json` | recommended | The body is parsed as JSON regardless. |
| `Idempotency-Key` | no | Up to 64 characters ([2.4](#24-idempotency-http)) |

**Body**

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `to` | string | yes | Indian mobile number in any form from [1.2](#12-destinations) |
| `sender` | string | yes | Registered DLT header ([1.3](#13-sender-header)) |
| `text` | string | yes | Message text (UTF-8). Encoding and segments are worked out by the platform ([1.5](#15-encoding-and-segments)). |
| `pe_id` | string | no | DLT Principal Entity ID. Found from the header when omitted. |
| `template_id` | string | no | DLT content template ID. Detected from the text when omitted. Sending it is faster and avoids ambiguity. |
| `tm_chain` | string | no | PE-TM chain value. The chain registered for the PE ID is used when omitted. |
| `client_ref` | string | no | Your reference, up to 64 characters. Returned in the status and the webhook. |

A missing `to`, `sender` or `text` fails the corresponding check (`invalid_destination`, `invalid_sender`, `invalid_text`).

HTTP messages always get a delivery receipt (webhook, [section 4](#4-delivery-receipts-for-http-customers)).

**Response `202 Accepted`**

| Field | Type | Description |
| --- | --- | --- |
| `id` | string | Message ID (UUID). Use it for `GET /v1/sms/{id}` and to match webhooks. |
| `segments` | integer | Segments billed |
| `cost` | string | Total held for the message, 4 decimal places |
| `status` | string | Always `accepted` |
| `duplicate` | boolean | Present and `true` only when the `Idempotency-Key` was seen before; `id` is the original message |

**Example**

```sh
curl -X POST https://api.example.com/v1/sms \
  -H "X-API-Key: $AMZ_KEY" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: order-58213-otp-1" \
  -d '{
        "to": "+919812345678",
        "sender": "AMZTST",
        "text": "Your OTP is 482913. Do not share it with anyone. -Amaze",
        "pe_id": "1101234567890123456",
        "template_id": "1107160000000012345",
        "client_ref": "order-58213"
      }'
```

```json
{"id":"0192b7a4-6f2e-7c1a-9d3e-5b8f2a1c4e77","segments":1,"cost":"0.1200","status":"accepted"}
```

Repeat with the same `Idempotency-Key`:

```json
{"id":"0192b7a4-6f2e-7c1a-9d3e-5b8f2a1c4e77","segments":1,"cost":"0.1200","status":"accepted","duplicate":true}
```

**Errors**

| HTTP | `code` | Meaning |
| --- | --- | --- |
| 400 | `invalid_json` | Body is not valid JSON, has an unknown field, or is over 64 KiB |
| 400 | `invalid_request` | `client_ref` or `Idempotency-Key` longer than 64 characters |
| 400 | `invalid_destination` | Not a valid Indian mobile number |
| 400 | `invalid_sender` | Sender empty, too long, or has characters other than letters and digits |
| 400 | `invalid_text` | Text empty, or longer than 10 segments |
| 401 | `unauthorized` | Missing, wrong or revoked key; or your account is suspended |
| 402 | `insufficient_balance` | `balance + credit_limit - held` is less than the cost |
| 402 | `reseller_billing` | Your reseller's wallet does not cover the message, or your reseller has no rate for this destination |
| 403 | `account_inactive` | Your account or your reseller's account is suspended |
| 422 | `no_rate` | No rate for this destination on your rate card |
| 422 | `dlt_entity`, `dlt_header`, `dlt_template`, `dlt_category`, `dlt_promo_hours`, `dlt_consent`, `dlt_tm_chain`, `dlt_url`, `dlt_callback` | DLT refusal ([1.4](#14-dlt-requirements-trai-tcccpr)); `error` names what is missing or does not match |
| 422 | `fw_sender`, `fw_content`, `fw_url`, `fw_destination`, `fw_destination_rate`, `fw_series` | Firewall refusal ([section 6](#6-error-codes-and-message-statuses)) |
| 429 | `throttled` | Over the account request rate |
| 503 | `unavailable` | Temporary platform problem. Nothing was held. Retry shortly, with the same `Idempotency-Key`. |

Example refusal:

```json
HTTP/1.1 422 Unprocessable Entity
{"code":"dlt_header","error":"dlt_header: header ABCDEF is not registered"}
```

### 3.3 `GET /v1/sms/{id}`: message status

Works for any message your account sent, over HTTP or SMPP, for **96 hours** after acceptance. After that it returns `404`; use the portal's message search for history.

**Response `200 OK`**

| Field | Type | Description |
| --- | --- | --- |
| `id` | string | Message ID |
| `status` | string | `accepted`, `submitted`, `delivered`, `failed`, `rejected` or `expired` ([6.4](#64-message-status-lifecycle)) |
| `error_code` | string | Present when there is one ([6.5](#65-error_code-values-status-api-and-webhook)) |
| `to` | string | Destination as `91XXXXXXXXXX` |
| `sender` | string | Sender as submitted |
| `segments` | integer | Segments billed |
| `cost` | string | Total held or charged for the message (decimal string; not padded to 4 places here) |
| `client_ref` | string | Present when you sent one (HTTP only) |
| `accepted_at` | string | When the platform accepted it |
| `completed_at` | string | Present once the status is final |

```sh
curl https://api.example.com/v1/sms/0192b7a4-6f2e-7c1a-9d3e-5b8f2a1c4e77 -H "X-API-Key: $AMZ_KEY"
```

```json
{
  "id": "0192b7a4-6f2e-7c1a-9d3e-5b8f2a1c4e77",
  "status": "delivered",
  "to": "919812345678",
  "sender": "AMZTST",
  "segments": 1,
  "cost": "0.12",
  "client_ref": "order-58213",
  "accepted_at": "2026-10-23T09:15:02.123Z",
  "completed_at": "2026-10-23T09:15:05.871Z"
}
```

**Errors:** `401 unauthorized`; `404 not_found` (unknown ID, another account's message, or older than 96 hours); `503 unavailable`.

### 3.4 `GET /v1/wallet`: balance and credit

**Response `200 OK`** (all amounts 4 decimal places)

| Field | Description |
| --- | --- |
| `currency` | `INR` |
| `balance` | Wallet balance; negative when credit is in use |
| `credit_limit` | Credit limit (0 unless raised by your provider) |
| `held` | Held for messages not yet settled |
| `available` | `balance + credit_limit - held`: what you can still spend |
| `credit_used` | How much of the credit limit is in use |

```sh
curl https://api.example.com/v1/wallet -H "X-API-Key: $AMZ_KEY"
```

```json
{"currency":"INR","balance":"4870.4400","credit_limit":"0.0000","held":"12.3600","available":"4858.0800","credit_used":"0.0000"}
```

**Errors:** `401 unauthorized`; `503 unavailable`.

---

## 4. Delivery receipts for HTTP customers

Messages sent over HTTP report their final status by webhook. (SMPP messages get `deliver_sm` receipts instead, [5.8](#58-delivery-receipts-deliver_sm). There is no webhook for SMPP messages and no `deliver_sm` for HTTP messages.)

### 4.1 Setting the webhook

Portal, **Settings > API & SMPP > Webhook**: enter an absolute `https://` (or `http://`) URL. Saving returns a new signing secret, shown **once**:

```
whsec_<64 hex characters>
```

Each save replaces the secret. Clearing the URL turns webhooks off. Changes reach the dispatcher within 1 minute, so for up to a minute events may still go to the old URL or be signed with the old secret.

The URL must resolve to a public address: the dispatcher refuses private, loopback, link-local, CGNAT and other non-public ranges when connecting. Redirects are not followed.

If no webhook is set when a message completes, its event is dropped (poll `GET /v1/sms/{id}` instead).

### 4.2 The request

One event per message, sent when the message reaches a final status (`delivered`, `failed`, `expired` or `rejected`). There is no event for `accepted` or `submitted`.

```
POST <your URL>
Content-Type: application/json
User-Agent: Amaze-Webhooks/1
X-Amaze-Event: message.status
X-Amaze-Delivery: 5d0f6b0e-8a52-4a8e-9a3b-0f5f7d2c11e4
X-Amaze-Signature: t=1761210905,v1=9c1e5f...b04a
```

```json
{
  "event": "message.status",
  "id": "0192b7a4-6f2e-7c1a-9d3e-5b8f2a1c4e77",
  "status": "failed",
  "error_code": "UNDELIV:1703|dlt_template:603",
  "to": "919812345678",
  "sender": "AMZTST",
  "segments": 1,
  "cost": "0.12",
  "client_ref": "order-58213",
  "accepted_at": "2026-10-23T09:15:02.123Z",
  "completed_at": "2026-10-23T09:15:05.871Z"
}
```

| Field | Description |
| --- | --- |
| `event` | Always `message.status` |
| `id` | Message ID returned by `POST /v1/sms` |
| `status` | `delivered`, `failed`, `expired` or `rejected` |
| `error_code` | Omitted when empty ([6.5](#65-error_code-values-status-api-and-webhook)) |
| `to`, `sender`, `segments`, `cost` | As in the status API |
| `client_ref` | Omitted when you sent none |
| `accepted_at`, `completed_at` | RFC 3339 UTC |

| Header | Description |
| --- | --- |
| `X-Amaze-Event` | `message.status` |
| `X-Amaze-Delivery` | A new random ID for **each attempt** (not stable across retries; de-duplicate by the body's `id`) |
| `X-Amaze-Signature` | `t=<unix seconds>,v1=<hex HMAC-SHA256>` |

### 4.3 Verifying the signature

`v1` is the lowercase hex HMAC-SHA256 of the string `<t>.<raw request body>`, keyed with your full webhook secret (including the `whsec_` prefix). `t` is the time of that attempt, so each retry carries a fresh timestamp.

To verify:

1. Read the **raw** body bytes before any JSON parsing.
2. Split the header on `,` and take `t` and `v1`.
3. Compute `HMAC-SHA256(secret, t + "." + body)` and compare it to `v1` in constant time.
4. Recommended: reject if `t` is more than a few minutes from your clock (replay protection). The platform does not enforce this; it is your choice.

**Python**

```python
import hashlib
import hmac
import time

def verify_amaze(secret: str, header: str, raw_body: bytes, tolerance_s: int = 300) -> bool:
    try:
        parts = dict(p.split("=", 1) for p in header.split(","))
        t, sig = parts["t"], parts["v1"]
    except (KeyError, ValueError):
        return False
    expected = hmac.new(secret.encode(), t.encode() + b"." + raw_body, hashlib.sha256).hexdigest()
    if not hmac.compare_digest(expected, sig):
        return False
    return abs(time.time() - int(t)) <= tolerance_s

# Flask
from flask import Flask, request, abort
app = Flask(__name__)
SECRET = "whsec_..."

@app.post("/sms-status")
def sms_status():
    if not verify_amaze(SECRET, request.headers.get("X-Amaze-Signature", ""), request.get_data()):
        abort(401)
    ev = request.get_json()
    # de-duplicate on ev["id"], then store ev["status"], ev.get("error_code")
    return "", 204
```

**Node.js**

```js
const crypto = require("crypto");
const express = require("express");

function verifyAmaze(secret, header, rawBody, toleranceS = 300) {
  const parts = Object.fromEntries(
    (header || "").split(",").map((p) => { const i = p.indexOf("="); return [p.slice(0, i), p.slice(i + 1)]; })
  );
  if (!parts.t || !parts.v1) return false;
  const expected = crypto.createHmac("sha256", secret)
    .update(parts.t + ".").update(rawBody).digest("hex");
  const a = Buffer.from(expected), b = Buffer.from(parts.v1);
  if (a.length !== b.length || !crypto.timingSafeEqual(a, b)) return false;
  return Math.abs(Date.now() / 1000 - Number(parts.t)) <= toleranceS;
}

const app = express();
const SECRET = "whsec_...";

// express.raw keeps the exact bytes that were signed
app.post("/sms-status", express.raw({ type: "application/json" }), (req, res) => {
  if (!verifyAmaze(SECRET, req.get("X-Amaze-Signature"), req.body)) return res.sendStatus(401);
  const ev = JSON.parse(req.body.toString("utf8"));
  // de-duplicate on ev.id, then store ev.status, ev.error_code
  res.sendStatus(204);
});

app.listen(3000);
```

### 4.4 Answering and retries

- Answer with any `2xx` within **10 seconds** (5 seconds to connect). Anything else (non-2xx, a `3xx` redirect, a timeout, a connection error) is a failed attempt.
- Failed events are retried after **30 s, 5 min, 30 min and 2 h**, each delay counted from the previous failure. That is 5 attempts in all. After the last failure the event is not delivered again.
- Delivery is **at least once**: the same event can arrive more than once. De-duplicate by `id`.
- Do slow work after answering.

### 4.5 Polling instead

`GET /v1/sms/{id}` ([3.3](#33-get-v1smsid-message-status)) returns the same status, `error_code` and times for 96 hours. Use it as a fallback for events your endpoint missed.

---

## 5. SMPP

### 5.1 Connection

| Item | Value |
| --- | --- |
| Host | `smpp.example.com` (placeholder) |
| Port, plain | `2775` |
| Port, TLS | `3550` |
| Versions | 3.3, 3.4, 5.0 (from `interface_version` in the bind: below `0x34` is treated as 3.3, `0x34` up to `0x4F` as 3.4, `0x50` and above as 5.0) |
| `system_id` / `password` | From your provider ([2.2](#22-smpp-credentials)); password at most 8 characters |
| `system_type`, `addr_ton`, `addr_npi`, `address_range` | Ignored |
| `bind_resp` `system_id` | `AMAZE` by default |
| `sc_interface_version` TLV in `bind_resp` | `0x34`, on 3.4 and 5.0 binds |
| IP allow list | Enforced per credential when set |

The load balancer passes your real address to the SMPP servers (PROXY protocol), so IP allow lists and per-address limits apply to your address.

### 5.2 Bind types

| Bind | Can submit | Gets delivery receipts |
| --- | --- | --- |
| `bind_transmitter` (0x00000002) | yes | no |
| `bind_receiver` (0x00000001) | no (`submit_sm` answered `0x04`) | yes |
| `bind_transceiver` (0x00000009) | yes | yes |

- Receipts are for the **account**, not the bind: a receipt can arrive on any receiver or transceiver bind of your account, whichever `system_id` and connection submitted the message. They are spread round-robin across your receiver-capable binds.
- If you have no receiver-capable bind, receipts wait and are delivered when you bind one.

### 5.3 Bind refusals and connection limits

Every bind refusal answers `0x0000000D` (ESME_RBINDFAIL) and closes the connection about 100 ms later. The platform deliberately gives the same answer for every cause:

- unknown `system_id` or wrong password;
- source address not in the credential's allow list;
- account suspended;
- `max_binds` for this `system_id` already in use (counted across all SMPP nodes);
- this address is in the failed-bind block (below).

| Limit | Value |
| --- | --- |
| Connections from one IP address (per SMPP node) | 64; more are closed at once |
| Time to bind after connecting | 10 s; an unbound connection is then closed |
| Failed binds | 10 failures in one minute from an address blocks binds from it for 5 minutes (per SMPP node). Fix your credentials before reconnecting in a loop. |
| Binds per connection | One. A second bind on a bound connection is answered `0x05` (ESME_RALYBND). |
| Bind slot after a crash | If an SMPP node dies, its bind slots free up within 90 s. |

### 5.4 Supported PDUs

| PDU | From you | From the platform |
| --- | --- | --- |
| `bind_transmitter`, `bind_receiver`, `bind_transceiver` | yes | |
| `submit_sm` | yes | |
| `deliver_sm` | | yes (receipts only) |
| `enquire_link` | yes, answered | yes, after 30 s of silence |
| `unbind` | yes, answered then closed | yes, on server shutdown |
| `generic_nack` | | for unknown commands (`0x03`) and malformed bodies (`0x01`) |

Not supported: `submit_multi`, `data_sm`, `query_sm`, `cancel_sm`, `replace_sm`, `alert_notification`. Unknown command IDs get `generic_nack` with `0x00000003`. A known request the server does not handle (for example `deliver_sm` from you) is answered with its response PDU carrying `0x00000003`.

MO (mobile-originated) messages are not delivered to customers yet.

### 5.5 `submit_sm`

**Fields read**

| Field | Use |
| --- | --- |
| `source_addr` | Sender / DLT header ([1.3](#13-sender-header)). Max 20 characters by SMPP framing; the platform's own limits are stricter. TON/NPI ignored. |
| `destination_addr` | Indian mobile number ([1.2](#12-destinations)). TON/NPI ignored. |
| `esm_class` | Only bit `0x40` (UDHI) is used: when set, the message starts with a user data header. |
| `registered_delivery` | Any value with bit 0 or bit 1 set (`0x01`, `0x02`, `0x03`) asks for a receipt. The receipt is sent for every final outcome, delivered or not; "failure only" (`0x02`) is not distinguished. `0x00`: no receipt. |
| `data_coding` | See below |
| `short_message` | Text; or `message_payload` TLV when present (it takes precedence) |
| DLT TLVs | See [5.6](#56-dlt-tlvs) |

`service_type`, `protocol_id`, `priority_flag`, `schedule_delivery_time`, `validity_period`, `replace_if_present_flag` and `sm_default_msg_id` are accepted and ignored: messages are sent at once, without a validity period of yours.

**`data_coding`**

| Value | Read as |
| --- | --- |
| `0x00` (SMSC default) | ISO-8859-1 (Latin-1 / ASCII). Send ASCII bytes; GSM-packed or GSM-unpacked septets are **not** decoded as GSM. |
| `0x01` (IA5/ASCII) | ISO-8859-1 |
| `0x03` (Latin-1) | ISO-8859-1 |
| `0x08` (UCS-2) | UTF-16 big-endian; the byte count must be even, else `0x01` |
| anything else | refused with `0x00000104` (ESME_RINVDCS) |

The platform re-encodes the text itself (GSM-7 when possible, else UCS-2) when sending it on, and counts segments from the decoded text ([1.5](#15-encoding-and-segments)).

**`submit_sm_resp`**

On success, `command_status` 0 and `message_id` = the platform's message ID, a 36-character UUID string such as `0192b7a4-6f2e-7c1a-9d3e-5b8f2a1c4e77`. The same ID is in the receipt and works with `GET /v1/sms/{id}`. Store it as a string. On failure, `command_status` is non-zero and there is no message ID ([5.11](#511-command_status-codes)).

Each `submit_sm` is answered only after the message is durably queued and its cost held. Acceptance is bounded at 3 seconds; if the platform cannot finish in time you get `0x14` (retry).

### 5.6 DLT TLVs

| Tag (default) | Field | Format |
| --- | --- | --- |
| `0x1400` | PE ID | ASCII string; a trailing NUL is allowed |
| `0x1401` | Template ID | ASCII string; a trailing NUL is allowed |
| `0x1402` | TM chain | ASCII string; a trailing NUL is allowed |

All three are optional ([1.4](#14-dlt-requirements-trai-tcccpr)), with one exception: a UDH part needs `0x1401`. The tag numbers are a deployment setting (`DLT_TLV_PE_ID`, `DLT_TLV_TEMPLATE_ID`, `DLT_TLV_TM_CHAIN`); ask your provider if theirs differ from the defaults. TLVs are read on every bind version.

### 5.7 Long messages and concatenation

There are three ways to send text longer than one SMS:

| Method | Supported | How it is handled |
| --- | --- | --- |
| **Whole text in `message_payload`** (TLV `0x0424`), `esm_class` without UDHI | **Yes. Recommended.** | One `submit_sm`, one message ID, one receipt. The platform splits it (up to 10 segments) and bills every segment. DLT matches the whole text. |
| **You split it, with a UDH** (`esm_class` `0x40`, UDH in `short_message`) | Yes, with rules below | Each part is its own message: own message ID, own receipt, billed as 1 segment. Sent to the operator with your UDH as is. |
| **SAR TLVs** (`sar_msg_ref_num` `0x020C`, `sar_total_segments` `0x020E`, `sar_segment_seqnum` `0x020F`) | **No** | The SAR TLVs are ignored. Each part is treated as a complete, separate message: it is DLT-matched as whole text (so a fragment usually fails `dlt_template`) and arrives on the handset as separate SMS. |

**UDH rules** (any breach is refused):

| Rule | Status if broken |
| --- | --- |
| The UDH length byte must fit inside the payload | `0x01` |
| The UDH holds exactly **one** information element, and it is concatenation: IEI `0x00` (8-bit reference, length 3) or IEI `0x08` (16-bit reference, length 4). No port addressing, WAP push, or other elements. | `0x43` (ESME_RINVESMCLASS) |
| Total parts 1 to 10; part number 1 to total | `0x43` |
| The part fits one SMS together with its UDH: GSM text up to 160 septets including the UDH (153 with a 6-byte UDH, 152 with a 7-byte UDH); UCS-2 up to 140 bytes including the UDH (67 characters with a 6-byte UDH) | `0x01` |
| The part carries a template ID TLV (`0x1401`), and its text could be a piece of that template | DLT `0x412` |

### 5.8 Delivery receipts (`deliver_sm`)

When a message submitted with `registered_delivery` reaches a final status, the platform sends one `deliver_sm` to a receiver-capable bind of your account. A message you sent as one `submit_sm` gets exactly one receipt, however many segments it had.

| Field | Value |
| --- | --- |
| `source_addr` | The destination number, `91XXXXXXXXXX` (TON/NPI 0) |
| `destination_addr` | Your sender (TON/NPI 0) |
| `esm_class` | `0x04` (SMSC delivery receipt) |
| `data_coding` | `0x00` |
| `short_message` | Receipt text, below |
| TLV `receipted_message_id` (`0x001E`) | The message ID, NUL-terminated (3.4 and 5.0 binds only) |
| TLV `message_state` (`0x0427`) | 1 byte: `2` DELIVRD, `3` EXPIRED, `5` UNDELIV, `8` REJECTD (3.4 and 5.0 binds only) |

On 3.3 binds the receipt has no TLVs; read the text.

**Receipt text** (SMPP 3.4 Appendix B style):

```
id:0192b7a4-6f2e-7c1a-9d3e-5b8f2a1c4e77 sub:001 dlvrd:001 submit date:2610230915 done date:2610230915 stat:DELIVRD err:000 text:
```

| Field | Value |
| --- | --- |
| `id` | Platform message ID (UUID) from `submit_sm_resp` |
| `sub` | Always `001` |
| `dlvrd` | `001` when `stat` is DELIVRD, else `000` |
| `submit date` | When the platform accepted the message, `YYMMDDhhmm`, **UTC** |
| `done date` | When the final status was reached, `YYMMDDhhmm`, UTC |
| `stat` | `DELIVRD`, `UNDELIV`, `EXPIRED` or `REJECTD` |
| `err` | 3 digits ([6.3](#63-receipt-err-codes)) |
| `text` | Always empty |

**`stat` mapping**

| Message status | `stat` |
| --- | --- |
| `delivered` | `DELIVRD` |
| `failed` | `UNDELIV` |
| `expired` | `EXPIRED` |
| `rejected` | `REJECTD` |

**Acknowledging receipts.** Answer each `deliver_sm` with `deliver_sm_resp`, status `0`, within 10 seconds. A receipt you do not acknowledge with status 0 (another status, `generic_nack`, a timeout, or the bind dropping) stays queued and is sent again after about a minute, to whichever of your receiver binds is up. Queued receipts survive SMPP server restarts. A receipt can therefore arrive more than once: de-duplicate on `id`.

### 5.9 `enquire_link` and idle connections

- Answer the platform's `enquire_link`. It sends one when the link has been silent for 30 seconds.
- A connection with nothing received for 90 seconds is closed.
- Your own `enquire_link` is answered immediately and counts as traffic. Every 30 seconds or so is fine.

### 5.10 Windowing and flow control

| Direction | Limit |
| --- | --- |
| Your requests in flight on one connection (submits, enquire_links, ...) | Up to 64 handled at once. Beyond that the server stops reading from the connection until replies go out, so keep your window at 64 or less and read your responses. |
| The platform's `deliver_sm` in flight on one bind | 10 |
| Response timeout (both directions) | 10 s |
| Maximum PDU size | 64 KiB. A larger `command_length` closes the connection. |
| Malformed body (bad C-string, string over its SMPP maximum, truncated TLV) | `generic_nack` `0x00000001`; the connection stays open |

SMPP C-octet string maxima (including the NUL): `system_id` 16, `password` 9, `system_type` 13, `address_range` 41, `service_type` 6, `source_addr` / `destination_addr` 21, `schedule_delivery_time` / `validity_period` 17.

### 5.11 `command_status` codes

**Standard codes the platform returns**

| Code | Name | When | What to do |
| --- | --- | --- | --- |
| `0x00000000` | ESME_ROK | Accepted | |
| `0x00000001` | ESME_RINVMSGLEN | Empty text; longer than 10 segments; a UDH part that does not fit one SMS; UDH length byte past the end; odd byte count with UCS-2. Also `generic_nack` for a malformed body. | Fix the message |
| `0x00000003` | ESME_RINVCMDID | Command not supported | Use only the PDUs in [5.4](#54-supported-pdus) |
| `0x00000004` | ESME_RINVBNDSTS | `submit_sm` before binding, or on a receiver bind | Bind as transmitter or transceiver |
| `0x00000005` | ESME_RALYBND | Bind on an already bound connection | One bind per connection |
| `0x00000008` | ESME_RSYSERR | Unexpected error | Retry later; contact support if it persists |
| `0x0000000A` | ESME_RINVSRCADR | Invalid sender | [1.3](#13-sender-header) |
| `0x0000000B` | ESME_RINVDSTADR | Not a valid Indian mobile number | [1.2](#12-destinations) |
| `0x0000000D` | ESME_RBINDFAIL | Any bind refusal ([5.3](#53-bind-refusals-and-connection-limits)) | Check credentials, IP, `max_binds`; wait out the block |
| `0x00000014` | ESME_RMSGQFUL | Temporary platform problem; nothing was held | Retry after a short delay |
| `0x00000043` | ESME_RINVESMCLASS | UDH not a single valid concatenation element | [5.7](#57-long-messages-and-concatenation) |
| `0x00000058` | ESME_RTHROTTLED | Over the bind's TPS | Slow down and retry |
| `0x00000104` | ESME_RINVDCS | Unsupported `data_coding` | Use 0x00, 0x01, 0x03 or 0x08 |

**Platform-specific codes** (SMPP reserves `0x400`-`0x4FF` for these)

| Code | Reason | Meaning |
| --- | --- | --- |
| `0x00000401` | insufficient_balance | Balance plus credit limit does not cover the cost |
| `0x00000402` | no_rate | No rate for this destination on your rate card |
| `0x00000403` | account_inactive | Your account or your reseller's account is suspended |
| `0x00000404` | reseller_billing | Your reseller's wallet is short, or it has no rate for this destination |
| `0x00000410` | dlt_entity | PE ID not registered for your account, or suspended |
| `0x00000411` | dlt_header | Sender not a registered header (under that PE ID), or suspended |
| `0x00000412` | dlt_template | No matching active template; wrong or suspended template ID; UDH part without template ID or not fitting it |
| `0x00000413` | dlt_category | Template category not allowed with this header type |
| `0x00000414` | dlt_promo_hours | Promotional outside 09:00-21:00 IST |
| `0x00000415` | dlt_consent | Service explicit template without consent template |
| `0x00000416` | dlt_tm_chain | No TM chain sent or registered |
| `0x00000417` | dlt_url | Link or APK not whitelisted on the template |
| `0x00000418` | dlt_callback | Callback number not whitelisted on the template |
| `0x00000420` | fw_sender | Sender on a block list |
| `0x00000421` | fw_content | Text matches a blocked content rule |
| `0x00000422` | fw_url | Link to a blocked domain |
| `0x00000423` | fw_destination | Number or range on a block list |
| `0x00000424` | fw_destination_rate | Too many messages to this number in the last hour |
| `0x00000425` | fw_series | Number range blocked for suspected SMS pumping |

---

## 6. Error codes and message statuses

### 6.1 Refusals at submit: one table for both interfaces

Refused messages are not queued, not charged, and get no receipt or webhook.

| Reason | HTTP | SMPP | Cause | What to do |
| --- | --- | --- | --- | --- |
| `invalid_json` | 400 | n/a | Bad JSON, unknown field, body over 64 KiB | Fix the request |
| `invalid_request` | 400 | n/a | `client_ref` or `Idempotency-Key` over 64 characters | Shorten it |
| `invalid_destination` | 400 | `0x0B` | Not an Indian mobile number | Check the number |
| `invalid_sender` | 400 | `0x0A` | Sender empty, over 11 characters (alphanumeric) or outside 3-15 digits (numeric), or has other characters | Use your registered header |
| `invalid_text` | 400 | `0x01` | Empty or over 10 segments | Shorten or split |
| (bad UDH) | n/a | `0x43` | UDH not one valid concatenation element | [5.7](#57-long-messages-and-concatenation) |
| (bad data_coding) | n/a | `0x104` | Unsupported DCS | [5.5](#55-submit_sm) |
| `unauthorized` | 401 | `0x0D` at bind | Bad, revoked or missing credentials; suspended account | Check credentials |
| `insufficient_balance` | 402 | `0x401` | Not enough available balance | Top up, or ask for a credit limit |
| `reseller_billing` | 402 | `0x404` | Your reseller's wallet or rate is missing | Contact your reseller |
| `account_inactive` | 403 | `0x403` | Your account or your reseller's is suspended | Contact your provider |
| `no_rate` | 422 | `0x402` | No rate for the destination | Contact your provider |
| `dlt_entity` | 422 | `0x410` | PE ID unknown or suspended | Register the PE ID, or send the right one |
| `dlt_header` | 422 | `0x411` | Header not registered (under that PE ID) or suspended | Register the header under the PE ID |
| `dlt_template` | 422 | `0x412` | Text matches no active template; template ID wrong for the header or suspended | Use the registered text exactly; check the template-header link |
| `dlt_category` | 422 | `0x413` | Promotional template on a non-numeric header, or the reverse | Use the right header type |
| `dlt_promo_hours` | 422 | `0x414` | Promotional outside 09:00-21:00 IST | Send within hours |
| `dlt_consent` | 422 | `0x415` | Service explicit template without consent template | Register the consent template |
| `dlt_tm_chain` | 422 | `0x416` | No TM chain | Send `tm_chain` / TLV `0x1402`, or register one |
| `dlt_url` | 422 | `0x417` | Link or APK not whitelisted | Whitelist it on the template |
| `dlt_callback` | 422 | `0x418` | Callback number not whitelisted | Whitelist it on the template |
| `fw_sender` | 422 | `0x420` | Sender blocked by the platform firewall | Contact support |
| `fw_content` | 422 | `0x421` | Content blocked | Contact support |
| `fw_url` | 422 | `0x422` | Domain blocked | Contact support |
| `fw_destination` | 422 | `0x423` | Number blocked | Contact support |
| `fw_destination_rate` | 422 | `0x424` | Over the per-number hourly limit (default 50) | Check for abuse of your OTP flow |
| `fw_series` | 422 | `0x425` | Number range blocked for suspected pumping | Check your traffic; your provider can unblock a range |
| `throttled` | 429 | `0x58` | Over the rate limit | Back off and retry |
| `unavailable` | 503 | `0x14` | Temporary platform problem | Retry (HTTP: same `Idempotency-Key`) |
| (unexpected) | n/a | `0x08` | Unexpected error | Retry; contact support if it persists |

### 6.2 Operator DLT scrubbing (after acceptance)

Operators still scrub accepted messages (for example against DND/NCPR for promotional traffic). These failures come back in the vendor's receipt. Your provider maps vendor codes to a reason and a 3-digit customer code. The DLT ones:

| Reason | Customer code |
| --- | --- |
| `dlt_other` (operator DLT failure without detail) | 600 |
| `dlt_entity` | 601 |
| `dlt_header` | 602 |
| `dlt_template` | 603 |
| `dlt_category` | 604 |
| `dlt_promo_hours` | 605 |
| `dlt_consent` | 606 |
| `dlt_tm_chain` | 607 |
| `dlt_url` | 608 |
| `dlt_callback` | 609 |
| `dlt_dnd` (recipient on DND/NCPR for this category) | 610 |

Your provider can also map non-DLT vendor codes (for example an invalid number) to other 3-digit codes. Ask for their list.

### 6.3 Receipt `err` codes

The `err:` field of an SMPP receipt is always 3 digits:

| `error_code` (status API) | Receipt `err` |
| --- | --- |
| empty (delivered) | `000` |
| has a mapped code, e.g. `UNDELIV:1703\|dlt_template:603` | the mapped code: `603` |
| operator code without mapping, e.g. `UNDELIV:34` | the operator code padded to 3 digits: `034`. Longer codes keep their **last** 3 digits (`UNDELIV:1703` gives `703`). |
| platform codes with no digits (`NO_DLR`, `NO_ROUTE`, `HLR_INVALID`, `HLR_ABSENT`, `SMPP_...`, `HTTP_...`, `VENDOR_...`) | `000` |

For the full detail of a failed SMPP message, look it up with `GET /v1/sms/{id}` (needs an API key on the same account) or in the portal's message search.

### 6.4 Message status lifecycle

```
              +--> rejected                      (platform refused it after acceptance)
              |
accepted -----+--> failed                        (vendor refused it at submit)
              |
              +--> submitted --+--> delivered
                               +--> failed       (operator receipt: undelivered, rejected, ...)
                               +--> expired      (operator receipt EXPIRED, or no receipt within 72 h)
```

| Status | Final | Meaning |
| --- | --- | --- |
| `accepted` | no | Queued, cost held |
| `submitted` | no | A vendor accepted it; the cost is now charged |
| `delivered` | yes | Every part delivered |
| `failed` | yes | The vendor refused it, or at least one part was not delivered |
| `rejected` | yes | The platform refused it after acceptance (no route; HLR says invalid or absent). Not charged. |
| `expired` | yes | Not every part was delivered and the last receipt to arrive said EXPIRED, or no receipt arrived within 72 hours (`error_code` `NO_DLR`) |

- A status never moves backwards, and a final status never changes.
- A multipart message is `delivered` only when every part is delivered; otherwise it is `failed` (or `expired` if the last receipt to arrive said EXPIRED), with the `error_code` taken from the last receipt to arrive.
- Only final statuses produce a receipt or webhook.

### 6.5 `error_code` values (status API and webhook)

| Pattern | Status | Meaning |
| --- | --- | --- |
| `NO_ROUTE` | `rejected` | No vendor route for the destination |
| `HLR_INVALID`, `HLR_ABSENT` | `rejected` | The route checks numbers by HLR, and the number is not in service, or absent for a long time |
| `SMPP_XXXXXXXX` | `failed` | The vendor's SMPP `submit_sm_resp` status, 8 uppercase hex digits (e.g. `SMPP_0000000B`) |
| `HTTP_<status>`, `HTTP_REFUSED`, `HTTP_BAD_RESPONSE`, `HTTP_NO_MESSAGE_ID`, `HTTP_BAD_REQUEST_TEMPLATE`, `VENDOR_<CODE>` | `failed` | An HTTP vendor refused the message, or its answer could not be used |
| `VENDOR_UNREACHABLE` | `failed` | The message could not be handed to the vendor |
| `<STAT>` or `<STAT>:<code>` | `failed` / `expired` | The operator's receipt, e.g. `UNDELIV:34`, `EXPIRED`, `REJECTD:001` |
| `<STAT>:<code>\|<reason>:<customer code>` | `failed` / `expired` | Same, with your provider's mapping added, e.g. `UNDELIV:1703\|dlt_template:603` |
| `NO_DLR` | `expired` | No receipt within 72 hours |

---

## 7. Quick start and FAQ

### 7.1 Quick start (HTTP)

1. Get your DLT registrations into the portal's DLT page: PE ID, headers and templates (CSV export from the operator portal works as is), and the TM chain. Use the page's check function to test a sender and text before going live.
2. Create an API key (**Settings > API & SMPP**) and store it.
3. Set a webhook URL there, and store the signing secret.
4. Check your balance:

   ```sh
   curl https://api.example.com/v1/wallet -H "X-API-Key: $AMZ_KEY"
   ```

5. Send:

   ```sh
   curl -X POST https://api.example.com/v1/sms \
     -H "X-API-Key: $AMZ_KEY" -H "Content-Type: application/json" \
     -H "Idempotency-Key: $(uuidgen)" \
     -d '{"to":"9812345678","sender":"AMZTST","text":"Test message 42","template_id":"1107160000000012346"}'
   ```

6. Receive the `message.status` webhook, verify its signature ([4.3](#43-verifying-the-signature)), and de-duplicate on `id`. Poll `GET /v1/sms/{id}` for anything you missed.

### 7.2 Quick start (SMPP)

1. Get a `system_id` and password from your provider, with your source IPs on the allow list.
2. Connect to port 3550 (TLS) or 2775, and send `bind_transceiver` with `interface_version` `0x34`.
3. Send `submit_sm`:

   | Field | Example |
   | --- | --- |
   | `source_addr` | `AMZTST` |
   | `destination_addr` | `919812345678` |
   | `esm_class` | `0x00` |
   | `registered_delivery` | `0x01` |
   | `data_coding` | `0x00` for ASCII text, `0x08` for UCS-2 |
   | `short_message` | up to 254 bytes; longer text goes in `message_payload` |
   | TLV `0x1400` | `1101234567890123456` (optional) |
   | TLV `0x1401` | `1107160000000012345` (optional, recommended) |

4. Keep the `message_id` from `submit_sm_resp` (a UUID string).
5. Answer each `deliver_sm` receipt with `deliver_sm_resp` status 0, and match it by `receipted_message_id` or the `id:` field.
6. Answer `enquire_link`, and send your own every 30 seconds.

### 7.3 FAQ

**Do I have to send `pe_id` and `template_id`?**
No. The PE ID is taken from the header and the template is detected from the text. Sending the template ID makes matching exact and quicker, and it is required for SMPP UDH parts.

**My message is refused with `dlt_template` but the text looks right.**
Literal text must match exactly, including case and punctuation; only whitespace runs are flexible. Each `{#var#}` takes at most 30 characters. Check that the template is linked to the header you send from, and that it is active. The `error` field (HTTP) says which case applies.

**I registered a template a moment ago and it is still refused.**
Registrations reach the sending nodes within 1 minute.

**How many segments will I pay for?**
See [1.5](#15-encoding-and-segments). A single non-GSM character (for example a curly quote or `₹`) switches the whole message to UCS-2 (70/67 characters per segment). The `segments` field in the `202` response tells you.

**Can I send my own concatenated parts over SMPP?**
Yes, with a single concatenation UDH per part and a template ID TLV on every part ([5.7](#57-long-messages-and-concatenation)). Each part gets its own message ID and receipt. SAR TLVs are not supported; use `message_payload` instead.

**Why do I get `0x0D` for every bind problem?**
On purpose: a different answer for a right password from a disallowed address would confirm the password. Check credentials, source IP, `max_binds`, and whether repeated failures have blocked your address for 5 minutes.

**Why are receipt dates off by 5:30?**
Receipt dates are UTC.

**Will I get a receipt for a message refused at submit?**
No. Only accepted messages get receipts or webhooks.

**I got the same webhook (or receipt) twice.**
Delivery is at least once. De-duplicate on the message `id`.

**My webhook endpoint was down. Are events lost?**
Each event is tried 5 times over about 2 h 35 min (30 s, 5 min, 30 min, 2 h). After that, use `GET /v1/sms/{id}` (available for 96 hours).

**Can I set a different webhook URL per message?**
No. There is one webhook URL per account.

**Can I schedule messages or set a validity period?**
No. `schedule_delivery_time` and `validity_period` are ignored, and HTTP has no such fields. Messages go out at once. A message without a final receipt is expired after 72 hours.

**Am I charged for undelivered messages?**
Yes, once a vendor has accepted the message (status `submitted`). Not for messages `rejected` by the platform or refused by the vendor before acceptance ([1.6](#16-billing)).

**Can I call the portal's API with my API key?**
No. API keys work only on the HTTP API in [section 3](#3-http-api). The portal's API needs a signed-in portal user.
