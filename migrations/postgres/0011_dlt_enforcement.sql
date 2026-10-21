-- Phase 3 slice 1: DLT enforcement and operator scrubbing codes.

-- 'off' skips DLT checks for an account (test accounts, or traffic DLT does
-- not cover). Only the platform changes it.
ALTER TABLE accounts ADD COLUMN dlt_mode text NOT NULL DEFAULT 'enforce' CHECK (dlt_mode IN ('enforce', 'off'));

-- What a vendor's receipt error codes mean. vendor_id NULL applies to every
-- vendor without its own row for the code. reason is a DLT reason
-- (dlt_template, dlt_dnd, ...) or any other short label; customer_code is
-- the err: value customers receive.
CREATE TABLE vendor_error_codes (
    id             bigserial PRIMARY KEY,
    vendor_id      text REFERENCES vendors(id),
    code           text NOT NULL,                 -- as the vendor sends it in err: (or the network error TLV)
    reason         text NOT NULL CHECK (reason ~ '^[a-z0-9_]{2,40}$'),
    customer_code  text NOT NULL CHECK (customer_code ~ '^[0-9]{3}$'),
    description    text NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX vendor_error_codes_key ON vendor_error_codes (coalesce(vendor_id, ''), code);
