-- HTTP vendors: the credential used in `auth` is stored encrypted
-- (SECRETS_KEY), the vendor calls our DLR endpoint with `dlr_token`, and
-- `concurrency` caps requests in flight.
ALTER TABLE http_vendor_profiles
    ADD COLUMN secret_enc  bytea,
    ADD COLUMN dlr_token   text NOT NULL DEFAULT encode(gen_random_bytes(16), 'hex'),
    ADD COLUMN concurrency int  NOT NULL DEFAULT 50 CHECK (concurrency > 0);
