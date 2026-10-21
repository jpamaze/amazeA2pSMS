-- Phase 4: portals (white-label branding) and alerts.

-- A reseller's (or the platform's own) brand. Sub-accounts see their
-- reseller's brand; a reseller's portal domain is a CNAME to the platform.
CREATE TABLE branding (
    account_id     uuid PRIMARY KEY REFERENCES accounts(id),
    domain         text UNIQUE CHECK (domain ~ '^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$'),
    display_name   text NOT NULL CHECK (length(display_name) BETWEEN 1 AND 60),
    logo           text NOT NULL DEFAULT '' CHECK (length(logo) <= 300000), -- https URL or data:image/...
    primary_color  text NOT NULL DEFAULT '#1f6feb' CHECK (primary_color ~ '^#[0-9a-fA-F]{6}$'),
    support_email  text NOT NULL DEFAULT '',
    updated_by     text NOT NULL,
    updated_at     timestamptz NOT NULL DEFAULT now()
);

-- Low-balance alerts: sent once when available (balance + credit limit -
-- held) drops below low_balance, re-armed when it is back at or above it.
CREATE TABLE alert_settings (
    account_id     uuid PRIMARY KEY REFERENCES accounts(id),
    low_balance    numeric(18,4) NOT NULL CHECK (low_balance >= 0),
    emails         text[] NOT NULL DEFAULT '{}',
    sms_numbers    text[] NOT NULL DEFAULT '{}',
    webhook_url    text NOT NULL DEFAULT '',
    enabled        boolean NOT NULL DEFAULT true,
    firing         boolean NOT NULL DEFAULT false,
    last_fired_at  timestamptz,
    updated_by     text NOT NULL,
    updated_at     timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE alert_events (
    id          bigserial PRIMARY KEY,
    account_id  uuid NOT NULL REFERENCES accounts(id),
    kind        text NOT NULL CHECK (kind IN ('low_balance', 'recovered')),
    available   numeric(18,4) NOT NULL,
    threshold   numeric(18,4) NOT NULL,
    delivered   text[] NOT NULL DEFAULT '{}',  -- channels that worked: email, sms, webhook
    errors      text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX alert_events_account ON alert_events (account_id, created_at DESC);
