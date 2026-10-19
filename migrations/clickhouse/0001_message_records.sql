-- One row per message state change; ReplacingMergeTree keeps the latest.
-- About 45 GB/day compressed at 5,000 TPS; 90 days online.
CREATE TABLE IF NOT EXISTS message_records
(
    message_id     UUID,
    account_id     UUID,
    reseller_id    Nullable(UUID),
    channel        LowCardinality(String),   -- smpp | http
    client_ref     String,
    sender         LowCardinality(String),
    destination    String,
    mcc            LowCardinality(String),
    mnc            LowCardinality(String),
    segments       UInt8,
    data_coding    UInt8,
    pe_id          LowCardinality(String),
    template_id    LowCardinality(String),
    dlt_category   LowCardinality(String),
    vendor_id      LowCardinality(String),
    vendor_msg_id  String,
    status         LowCardinality(String),
    error_code     LowCardinality(String),
    price          Decimal(18, 4),
    cost           Decimal(18, 4),
    accepted_at    DateTime64(3, 'Asia/Kolkata'),
    submitted_at   Nullable(DateTime64(3, 'Asia/Kolkata')),
    done_at        Nullable(DateTime64(3, 'Asia/Kolkata')),
    updated_at     DateTime64(3, 'Asia/Kolkata')
)
ENGINE = ReplacingMergeTree(updated_at)
PARTITION BY toYYYYMMDD(accepted_at)
ORDER BY (account_id, accepted_at, message_id)
TTL toDateTime(accepted_at) + INTERVAL 90 DAY;
