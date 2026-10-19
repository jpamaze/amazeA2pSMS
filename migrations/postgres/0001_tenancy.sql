-- Accounts form a tree: platform -> resellers -> customers (resellers may nest).
CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE accounts (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    parent_id   uuid REFERENCES accounts(id),
    role        text NOT NULL CHECK (role IN ('platform', 'reseller', 'customer')),
    name        text NOT NULL,
    status      text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'suspended', 'closed')),
    created_at  timestamptz NOT NULL DEFAULT now(),
    CHECK ((role = 'platform') = (parent_id IS NULL))
);
CREATE INDEX accounts_parent_idx ON accounts (parent_id);

-- Only the platform or a reseller may own sub-accounts.
CREATE FUNCTION accounts_check_parent() RETURNS trigger AS $$
DECLARE
    parent_role text;
BEGIN
    IF NEW.parent_id IS NULL THEN
        RETURN NEW;
    END IF;
    SELECT role INTO parent_role FROM accounts WHERE id = NEW.parent_id;
    IF parent_role NOT IN ('platform', 'reseller') THEN
        RAISE EXCEPTION 'parent account % is a %, only platform or reseller may own sub-accounts',
            NEW.parent_id, parent_role USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER accounts_check_parent
    BEFORE INSERT OR UPDATE OF parent_id ON accounts
    FOR EACH ROW EXECUTE FUNCTION accounts_check_parent();

CREATE TABLE users (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id     uuid NOT NULL REFERENCES accounts(id),
    email          text NOT NULL UNIQUE,
    password_hash  text NOT NULL,
    role           text NOT NULL CHECK (role IN ('owner', 'admin', 'finance', 'support', 'viewer')),
    totp_secret    text,
    created_at     timestamptz NOT NULL DEFAULT now(),
    disabled_at    timestamptz
);

-- HTTP API keys: only a hash is stored; the prefix identifies the key in logs.
CREATE TABLE api_keys (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id  uuid NOT NULL REFERENCES accounts(id),
    prefix      text NOT NULL UNIQUE,
    key_hash    text NOT NULL,
    name        text NOT NULL,
    scopes      text[] NOT NULL DEFAULT '{sms:send}',
    allowed_ips cidr[],
    created_at  timestamptz NOT NULL DEFAULT now(),
    revoked_at  timestamptz
);

-- Customer SMPP binds.
CREATE TABLE smpp_credentials (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id     uuid NOT NULL REFERENCES accounts(id),
    system_id      text NOT NULL UNIQUE CHECK (length(system_id) <= 16),
    password_hash  text NOT NULL,
    allowed_ips    cidr[],
    max_binds      int  NOT NULL DEFAULT 4 CHECK (max_binds > 0),
    tps_limit      int  NOT NULL DEFAULT 100 CHECK (tps_limit > 0),
    created_at     timestamptz NOT NULL DEFAULT now(),
    disabled_at    timestamptz
);
