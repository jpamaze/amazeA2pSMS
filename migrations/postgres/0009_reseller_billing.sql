-- Phase 2: reseller-owned rate cards, wallet transfers, message counts on charges.

-- NULL owner = a platform rate card; otherwise the reseller that owns it and
-- may assign it to its sub-accounts.
ALTER TABLE rate_cards ADD COLUMN owner_account_id uuid REFERENCES accounts(id);
CREATE UNIQUE INDEX rate_cards_owner_name ON rate_cards (coalesce(owner_account_id::text, ''), name);

ALTER TABLE ledger_entries DROP CONSTRAINT ledger_entries_kind_check;
ALTER TABLE ledger_entries ADD CONSTRAINT ledger_entries_kind_check
    CHECK (kind IN ('topup', 'charge', 'refund', 'adjustment', 'transfer_in', 'transfer_out'));
ALTER TABLE ledger_entries ADD COLUMN quantity int;              -- messages, for charge entries
ALTER TABLE ledger_entries ADD COLUMN counterparty uuid REFERENCES accounts(id); -- for transfers
