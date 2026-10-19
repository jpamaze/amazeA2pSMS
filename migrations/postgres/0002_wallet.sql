-- Wallet and credit limit. There is no postpaid billing:
--   available = balance + credit_limit - open holds (holds live in Redis)
-- credit_limit defaults to 0, so every new account is prepaid only.
-- Amounts have 4 decimal places (Rs 0.1234 per SMS).

CREATE TABLE wallets (
    account_id    uuid PRIMARY KEY REFERENCES accounts(id),
    currency      char(3)       NOT NULL DEFAULT 'INR',
    balance       numeric(18,4) NOT NULL DEFAULT 0,
    credit_limit  numeric(18,4) NOT NULL DEFAULT 0 CHECK (credit_limit >= 0),
    -- balance may be negative (credit used). It is not constrained to
    -- >= -credit_limit because lowering a limit below credit already used is
    -- allowed; new holds are then refused until a top-up.
    updated_at    timestamptz   NOT NULL DEFAULT now()
);

-- Every account gets a wallet with credit limit 0.
CREATE FUNCTION accounts_create_wallet() RETURNS trigger AS $$
BEGIN
    INSERT INTO wallets (account_id) VALUES (NEW.id);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER accounts_create_wallet
    AFTER INSERT ON accounts
    FOR EACH ROW EXECUTE FUNCTION accounts_create_wallet();

CREATE TABLE ledger_entries (
    id               bigserial PRIMARY KEY,
    account_id       uuid          NOT NULL REFERENCES accounts(id),
    kind             text          NOT NULL CHECK (kind IN ('topup', 'charge', 'refund', 'adjustment')),
    amount           numeric(18,4) NOT NULL, -- signed: + credits the wallet, - debits it
    balance_after    numeric(18,4) NOT NULL,
    reference        text,
    idempotency_key  text          NOT NULL UNIQUE,
    created_by       text,
    created_at       timestamptz   NOT NULL DEFAULT now()
);
CREATE INDEX ledger_entries_account_idx ON ledger_entries (account_id, created_at DESC);

-- Audit of every credit limit change: who, why, old and new.
CREATE TABLE credit_limit_changes (
    id          bigserial PRIMARY KEY,
    account_id  uuid          NOT NULL REFERENCES accounts(id),
    old_limit   numeric(18,4) NOT NULL,
    new_limit   numeric(18,4) NOT NULL,
    changed_by  text          NOT NULL,
    reason      text          NOT NULL,
    changed_at  timestamptz   NOT NULL DEFAULT now()
);
CREATE INDEX credit_limit_changes_account_idx ON credit_limit_changes (account_id, changed_at DESC);

-- Credit limit rules, enforced on every change:
--  1. A reseller may give its sub-accounts credit only within its own
--     available balance: sum(children credit_limit) <= reseller balance + reseller credit_limit.
--  2. The change is logged. The caller sets app.actor and app.reason in the
--     same transaction (SET LOCAL); a change without them is rejected.
CREATE FUNCTION wallets_credit_limit_rules() RETURNS trigger AS $$
DECLARE
    parent       uuid;
    parent_role  text;
    parent_avail numeric(18,4);
    children_sum numeric(18,4);
    actor        text := nullif(current_setting('app.actor', true), '');
    why          text := nullif(current_setting('app.reason', true), '');
BEGIN
    IF NEW.credit_limit = OLD.credit_limit THEN
        RETURN NEW;
    END IF;

    IF actor IS NULL OR why IS NULL THEN
        RAISE EXCEPTION 'credit limit change needs app.actor and app.reason'
            USING ERRCODE = 'check_violation';
    END IF;

    SELECT a.parent_id, p.role INTO parent, parent_role
    FROM accounts a JOIN accounts p ON p.id = a.parent_id
    WHERE a.id = NEW.account_id;

    IF parent_role = 'reseller' AND NEW.credit_limit > OLD.credit_limit THEN
        -- Lock the reseller's wallet so two sub-account changes cannot race.
        SELECT balance + credit_limit INTO parent_avail
        FROM wallets WHERE account_id = parent FOR UPDATE;

        SELECT coalesce(sum(w.credit_limit), 0) INTO children_sum
        FROM wallets w JOIN accounts a ON a.id = w.account_id
        WHERE a.parent_id = parent AND w.account_id <> NEW.account_id;

        IF children_sum + NEW.credit_limit > parent_avail THEN
            RAISE EXCEPTION 'reseller % available % cannot cover sub-account credit limits % (+%)',
                parent, parent_avail, children_sum, NEW.credit_limit
                USING ERRCODE = 'check_violation';
        END IF;
    END IF;

    INSERT INTO credit_limit_changes (account_id, old_limit, new_limit, changed_by, reason)
    VALUES (NEW.account_id, OLD.credit_limit, NEW.credit_limit, actor, why);

    NEW.updated_at := now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER wallets_credit_limit_rules
    BEFORE UPDATE OF credit_limit ON wallets
    FOR EACH ROW EXECUTE FUNCTION wallets_credit_limit_rules();
