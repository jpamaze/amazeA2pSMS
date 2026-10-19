-- India DLT (TRAI TCCCPR). Every message must match a registered entity,
-- header and template, and carry the telemarketer chain.

CREATE TABLE dlt_entities (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id  uuid NOT NULL REFERENCES accounts(id),
    pe_id       text NOT NULL UNIQUE,          -- Principal Entity ID
    name        text NOT NULL,
    status      text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'suspended')),
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX dlt_entities_account_idx ON dlt_entities (account_id);

CREATE TABLE dlt_headers (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    entity_id   uuid NOT NULL REFERENCES dlt_entities(id),
    header      text NOT NULL,                 -- sender ID
    category    text NOT NULL CHECK (category IN ('transactional', 'service_implicit', 'service_explicit', 'promotional')),
    status      text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'suspended')),
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (entity_id, header)
);

CREATE TABLE dlt_consent_templates (
    id                   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    entity_id            uuid NOT NULL REFERENCES dlt_entities(id),
    consent_template_id  text NOT NULL UNIQUE,
    body                 text NOT NULL,
    created_at           timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE dlt_templates (
    id                   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    entity_id            uuid NOT NULL REFERENCES dlt_entities(id),
    template_id          text NOT NULL UNIQUE,  -- DLT content template ID
    name                 text NOT NULL,
    body                 text NOT NULL,         -- with {#var#} placeholders
    category             text NOT NULL CHECK (category IN ('transactional', 'service_implicit', 'service_explicit', 'promotional')),
    consent_template_id  uuid REFERENCES dlt_consent_templates(id),
    status               text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'suspended')),
    created_at           timestamptz NOT NULL DEFAULT now(),
    CHECK (category <> 'service_explicit' OR consent_template_id IS NOT NULL)
);

-- A template is registered against one or more headers.
CREATE TABLE dlt_template_headers (
    template_id  uuid NOT NULL REFERENCES dlt_templates(id),
    header_id    uuid NOT NULL REFERENCES dlt_headers(id),
    PRIMARY KEY (template_id, header_id)
);

-- URLs, APK links and callback numbers allowed inside a template's variables.
CREATE TABLE dlt_template_whitelist (
    id           bigserial PRIMARY KEY,
    template_id  uuid NOT NULL REFERENCES dlt_templates(id),
    kind         text NOT NULL CHECK (kind IN ('url', 'apk', 'callback')),
    value        text NOT NULL,
    UNIQUE (template_id, kind, value)
);

-- Telemarketers in the delivery chain; is_self marks this platform's own TM ID.
CREATE TABLE telemarketers (
    id       uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tm_id    text NOT NULL UNIQUE,
    name     text NOT NULL,
    is_self  boolean NOT NULL DEFAULT false
);
CREATE UNIQUE INDEX telemarketers_one_self ON telemarketers (is_self) WHERE is_self;

-- PE-TM chain value per entity, as registered on the DLT platform.
CREATE TABLE dlt_tm_chains (
    id           bigserial PRIMARY KEY,
    entity_id    uuid NOT NULL REFERENCES dlt_entities(id),
    chain_value  text NOT NULL,
    valid_from   timestamptz NOT NULL DEFAULT now(),
    valid_to     timestamptz
);
CREATE INDEX dlt_tm_chains_entity_idx ON dlt_tm_chains (entity_id, valid_from DESC);
