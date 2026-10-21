-- Customer webhooks: delivery status is POSTed to webhook_url, signed with
-- HMAC-SHA256 using the account's secret (stored encrypted, SECRETS_KEY).
ALTER TABLE accounts
    ADD COLUMN webhook_url        text,
    ADD COLUMN webhook_secret_enc bytea;
