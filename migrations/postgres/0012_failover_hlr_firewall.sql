-- Phase 3 slice 2: vendor failover, HLR/MNP, firewall.

-- When a vendor is taken out of routing (internal/vendorhealth).
ALTER TABLE vendors
    ADD COLUMN failover_enabled        boolean NOT NULL DEFAULT true,
    ADD COLUMN failover_max_submit_ms  int NOT NULL DEFAULT 3000 CHECK (failover_max_submit_ms >= 0),  -- p95; 0 = off
    ADD COLUMN failover_max_error_pct  int NOT NULL DEFAULT 50 CHECK (failover_max_error_pct BETWEEN 0 AND 100), -- 0 = off
    ADD COLUMN failover_min_dlr_pct    int NOT NULL DEFAULT 0 CHECK (failover_min_dlr_pct BETWEEN 0 AND 100),    -- 0 = off
    ADD COLUMN failover_dlr_window_min int NOT NULL DEFAULT 15 CHECK (failover_dlr_window_min BETWEEN 1 AND 120),
    ADD COLUMN failover_cooldown_s     int NOT NULL DEFAULT 30 CHECK (failover_cooldown_s >= 0);

-- Operator number series (national prefix -> network), from the DoT
-- numbering plan; longest prefix wins. Ported numbers are in Redis (MNP).
CREATE TABLE number_series (
    prefix    text PRIMARY KEY CHECK (prefix ~ '^[0-9]{2,9}$'),
    mcc       text NOT NULL CHECK (mcc ~ '^[0-9]{3}$'),
    mnc       text NOT NULL CHECK (mnc ~ '^[0-9]{2,3}$'),
    operator  text NOT NULL DEFAULT '',
    circle    text NOT NULL DEFAULT ''
);

CREATE TABLE mnp_imports (
    id          bigserial PRIMARY KEY,
    rows        bigint NOT NULL,
    bad_lines   int NOT NULL,
    imported_by text NOT NULL,
    imported_at timestamptz NOT NULL DEFAULT now()
);

-- HLR per route: look the number up before choosing a vendor, and
-- optionally refuse numbers the HLR reports invalid or absent.
ALTER TABLE routes
    ADD COLUMN hlr        boolean NOT NULL DEFAULT false,
    ADD COLUMN hlr_reject boolean NOT NULL DEFAULT false;

-- Firewall block lists (internal/firewall). account_id NULL = every account.
CREATE TABLE firewall_rules (
    id          bigserial PRIMARY KEY,
    account_id  uuid REFERENCES accounts(id),
    kind        text NOT NULL CHECK (kind IN ('sender', 'content', 'url', 'destination')),
    pattern     text NOT NULL CHECK (length(pattern) BETWEEN 1 AND 500),
    action      text NOT NULL DEFAULT 'reject' CHECK (action IN ('reject', 'flag')),
    note        text NOT NULL DEFAULT '',
    enabled     boolean NOT NULL DEFAULT true,
    created_by  text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);

-- Per-account velocity limits; accounts without a row get the defaults
-- (50 per number per hour, 300 per 1,000-number range per 10 minutes,
-- range blocked 60 minutes). 0 turns a limit off.
CREATE TABLE firewall_limits (
    account_id            uuid PRIMARY KEY REFERENCES accounts(id),
    dest_per_hour         int NOT NULL CHECK (dest_per_hour >= 0),
    series_per_10min      int NOT NULL CHECK (series_per_10min >= 0),
    series_block_minutes  int NOT NULL CHECK (series_block_minutes >= 0),
    updated_by            text NOT NULL,
    updated_at            timestamptz NOT NULL DEFAULT now()
);
