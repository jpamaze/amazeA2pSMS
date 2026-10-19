-- Vendors, binds, operators, prices and routes.

CREATE TABLE vendors (
    id          text PRIMARY KEY CHECK (id ~ '^[a-z0-9-]{2,32}$'), -- also names the Kafka route topic
    name        text NOT NULL,
    protocol    text NOT NULL CHECK (protocol IN ('smpp', 'http')),
    status      text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'paused', 'disabled')),
    dlr_timeout interval NOT NULL DEFAULT '72 hours', -- reconciliation marks messages expired after this
    created_at  timestamptz NOT NULL DEFAULT now()
);

-- Outbound SMPP binds. Version, TLVs and limits are per bind.
CREATE TABLE vendor_binds (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    vendor_id     text NOT NULL REFERENCES vendors(id),
    host          text NOT NULL,
    port          int  NOT NULL CHECK (port BETWEEN 1 AND 65535),
    tls           boolean NOT NULL DEFAULT false,
    system_id     text NOT NULL,
    password_enc  bytea NOT NULL,           -- encrypted with the platform KMS key
    system_type   text NOT NULL DEFAULT '',
    smpp_version  text NOT NULL DEFAULT '3.4' CHECK (smpp_version IN ('3.3', '3.4', '5.0')),
    bind_type     text NOT NULL DEFAULT 'transceiver' CHECK (bind_type IN ('transmitter', 'receiver', 'transceiver')),
    window_size   int  NOT NULL DEFAULT 10 CHECK (window_size > 0),
    tps_limit     int  NOT NULL DEFAULT 100 CHECK (tps_limit > 0),
    -- Which TLV tags carry DLT PE ID, template ID and TM chain for this vendor,
    -- e.g. {"pe_id": "0x1400", "template_id": "0x1401", "tm_chain": "0x1402"}.
    tlv_profile   jsonb NOT NULL DEFAULT '{}',
    enabled       boolean NOT NULL DEFAULT true
);
CREATE INDEX vendor_binds_vendor_idx ON vendor_binds (vendor_id);

-- Outbound HTTP vendors: request shape and DLR mapping are config.
CREATE TABLE http_vendor_profiles (
    vendor_id         text PRIMARY KEY REFERENCES vendors(id),
    base_url          text  NOT NULL,
    auth              jsonb NOT NULL DEFAULT '{}',
    request_template  jsonb NOT NULL,
    response_mapping  jsonb NOT NULL,
    dlr_mapping       jsonb NOT NULL,
    tps_limit         int   NOT NULL DEFAULT 100 CHECK (tps_limit > 0)
);

CREATE TABLE operators (
    mcc           text NOT NULL,
    mnc           text NOT NULL,
    country_code  char(2) NOT NULL,
    name          text NOT NULL,
    PRIMARY KEY (mcc, mnc)
);

-- What customers pay.
CREATE TABLE rate_cards (
    id        uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name      text NOT NULL,
    currency  char(3) NOT NULL DEFAULT 'INR'
);

CREATE TABLE rate_card_entries (
    rate_card_id  uuid NOT NULL REFERENCES rate_cards(id),
    mcc           text NOT NULL,
    mnc           text NOT NULL,          -- '*' for every network in the country
    category      text NOT NULL DEFAULT '*',
    price         numeric(18,4) NOT NULL CHECK (price >= 0),
    valid_from    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (rate_card_id, mcc, mnc, category, valid_from)
);

ALTER TABLE accounts ADD COLUMN rate_card_id uuid REFERENCES rate_cards(id);

-- What vendors charge us; LCR picks from these.
CREATE TABLE vendor_costs (
    vendor_id   text NOT NULL REFERENCES vendors(id),
    mcc         text NOT NULL,
    mnc         text NOT NULL,
    category    text NOT NULL DEFAULT '*',
    cost        numeric(18,4) NOT NULL CHECK (cost >= 0),
    valid_from  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (vendor_id, mcc, mnc, category, valid_from)
);

-- Route rules. account_id NULL = platform default.
CREATE TABLE routes (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id  uuid REFERENCES accounts(id),
    mcc         text NOT NULL,
    mnc         text NOT NULL DEFAULT '*',
    category    text NOT NULL DEFAULT '*',
    mode        text NOT NULL DEFAULT 'lcr' CHECK (mode IN ('lcr', 'priority', 'weighted')),
    vendor_id   text REFERENCES vendors(id),  -- NULL with mode = lcr
    priority    int  NOT NULL DEFAULT 100,
    weight      int  NOT NULL DEFAULT 1 CHECK (weight > 0),
    enabled     boolean NOT NULL DEFAULT true
);
CREATE INDEX routes_lookup_idx ON routes (account_id, mcc, mnc, category) WHERE enabled;
