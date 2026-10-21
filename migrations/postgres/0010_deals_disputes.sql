-- Phase 2 slice 2: customer and vendor deals, customer and vendor disputes.

-- A special price for one account on one destination for a period, set by
-- the account's parent (the platform, or the reseller that owns it). With a
-- cap, the deal price applies until cap_segments segments are used; then the
-- rate card applies again.
CREATE TABLE customer_deals (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id        uuid NOT NULL REFERENCES accounts(id),
    granted_by        uuid REFERENCES accounts(id),       -- NULL = platform
    name              text NOT NULL,
    mcc               text NOT NULL,
    mnc               text NOT NULL DEFAULT '*',
    price             numeric(18,4) NOT NULL CHECK (price >= 0),
    cap_segments      bigint CHECK (cap_segments > 0),     -- NULL = no cap
    used_segments     bigint NOT NULL DEFAULT 0,           -- charged; kept by the rater
    starts_at         timestamptz NOT NULL,
    ends_at           timestamptz NOT NULL,
    status            text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'cancelled')),
    created_by        text NOT NULL,
    created_at        timestamptz NOT NULL DEFAULT now(),
    CHECK (ends_at > starts_at)
);
CREATE INDEX customer_deals_lookup ON customer_deals (account_id, mcc, ends_at) WHERE status = 'active';

-- A vendor gives us a lower cost on a destination for a period in return for
-- committing volume. Routing prefers the vendor until the commitment is met;
-- missing volume at the end may cost shortfall_rate per segment.
CREATE TABLE vendor_deals (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    vendor_id         text NOT NULL REFERENCES vendors(id),
    name              text NOT NULL,
    mcc               text NOT NULL,
    mnc               text NOT NULL DEFAULT '*',
    cost              numeric(18,4) NOT NULL CHECK (cost >= 0),
    commit_segments   bigint NOT NULL CHECK (commit_segments > 0),
    shortfall_rate    numeric(18,4) NOT NULL DEFAULT 0 CHECK (shortfall_rate >= 0),
    starts_at         timestamptz NOT NULL,
    ends_at           timestamptz NOT NULL,
    status            text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'cancelled')),
    created_by        text NOT NULL,
    created_at        timestamptz NOT NULL DEFAULT now(),
    CHECK (ends_at > starts_at)
);
CREATE INDEX vendor_deals_active ON vendor_deals (ends_at) WHERE status = 'active';

-- A customer (or reseller) claims it was wrongly charged. The account's
-- parent resolves it; an approved amount is credited to the wallet.
CREATE TABLE customer_disputes (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id        uuid NOT NULL REFERENCES accounts(id),
    resolver_id       uuid REFERENCES accounts(id),        -- the parent; NULL = platform
    period_from       timestamptz NOT NULL,
    period_to         timestamptz NOT NULL,
    message_ids       uuid[] NOT NULL DEFAULT '{}',
    claimed_amount    numeric(18,4) NOT NULL CHECK (claimed_amount > 0),
    reason            text NOT NULL,
    status            text NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'approved', 'rejected', 'withdrawn')),
    approved_amount   numeric(18,4) CHECK (approved_amount > 0),
    resolution        text,
    raised_by         text NOT NULL,
    resolved_by       text,
    created_at        timestamptz NOT NULL DEFAULT now(),
    resolved_at       timestamptz,
    CHECK (period_to > period_from),
    CHECK (approved_amount IS NULL OR approved_amount <= claimed_amount)
);
CREATE INDEX customer_disputes_account ON customer_disputes (account_id, created_at DESC);
CREATE INDEX customer_disputes_resolver ON customer_disputes (resolver_id, status);

-- What a vendor billed us, per day (IST) and destination.
CREATE TABLE vendor_invoices (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    vendor_id         text NOT NULL REFERENCES vendors(id),
    reference         text NOT NULL,
    period_from       date NOT NULL,
    period_to         date NOT NULL,                       -- inclusive
    total_segments    bigint NOT NULL,
    total_amount      numeric(18,4) NOT NULL,
    imported_by       text NOT NULL,
    imported_at       timestamptz NOT NULL DEFAULT now(),
    UNIQUE (vendor_id, reference),
    CHECK (period_to >= period_from)
);

CREATE TABLE vendor_invoice_lines (
    invoice_id        uuid NOT NULL REFERENCES vendor_invoices(id) ON DELETE CASCADE,
    day               date NOT NULL,
    mcc               text NOT NULL,
    mnc               text NOT NULL,
    segments          bigint NOT NULL CHECK (segments >= 0),
    amount            numeric(18,4) NOT NULL CHECK (amount >= 0),
    PRIMARY KEY (invoice_id, day, mcc, mnc)
);

-- A dispute raised with a vendor over an invoice, tracked to settlement.
CREATE TABLE vendor_disputes (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    invoice_id        uuid NOT NULL REFERENCES vendor_invoices(id),
    status            text NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'sent', 'accepted', 'rejected', 'settled')),
    disputed_amount   numeric(18,4) NOT NULL CHECK (disputed_amount > 0),
    settled_amount    numeric(18,4) CHECK (settled_amount >= 0),  -- credit note received
    lines             jsonb NOT NULL,                      -- the mismatched lines when raised
    notes             text NOT NULL DEFAULT '',
    created_by        text NOT NULL,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);
-- One dispute per invoice that is still being worked.
CREATE UNIQUE INDEX vendor_disputes_one_open ON vendor_disputes (invoice_id) WHERE status IN ('open', 'sent');

-- Dispute credits: 'refund' on the claimant, 'refund_given' on a reseller
-- that pays it out of its own wallet.
ALTER TABLE ledger_entries DROP CONSTRAINT ledger_entries_kind_check;
ALTER TABLE ledger_entries ADD CONSTRAINT ledger_entries_kind_check
    CHECK (kind IN ('topup', 'charge', 'refund', 'refund_given', 'adjustment', 'transfer_in', 'transfer_out'));
