-- Every account charged for the message (sender and resellers above it),
-- account ID -> amount. Used for reseller reporting and billing checks.
ALTER TABLE message_records ADD COLUMN IF NOT EXISTS charges Map(String, Decimal(18, 4));
