-- Who suspended an account: only that account (or the platform) may lift
-- the suspension, so a reseller cannot undo a platform suspension of its
-- sub-account.
ALTER TABLE accounts ADD COLUMN IF NOT EXISTS suspended_by uuid REFERENCES accounts(id);
