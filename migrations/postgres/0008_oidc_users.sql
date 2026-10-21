-- Staff and reseller users sign in through Keycloak (OIDC). Passwords and
-- 2FA live in Keycloak; a platform user is matched by verified email.
ALTER TABLE users ALTER COLUMN password_hash DROP NOT NULL;
ALTER TABLE users DROP CONSTRAINT users_email_key;
CREATE UNIQUE INDEX users_email_lower_key ON users (lower(email));
ALTER TABLE users ADD COLUMN last_login_at timestamptz;

-- Every admin API change, who made it and from where.
CREATE TABLE admin_audit (
    id          bigserial PRIMARY KEY,
    at          timestamptz NOT NULL DEFAULT now(),
    actor_email text NOT NULL,
    actor_acct  uuid NOT NULL,
    method      text NOT NULL,
    path        text NOT NULL,
    status      int  NOT NULL,
    remote_ip   text
);
CREATE INDEX admin_audit_at_idx ON admin_audit (at DESC);
