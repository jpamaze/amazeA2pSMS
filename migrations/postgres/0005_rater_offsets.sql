-- The rater writes ledger batches exactly once: each batch transaction also
-- records the last Kafka offset it covered per partition, and a replayed
-- batch skips records at or below it.
CREATE TABLE rater_offsets (
    topic       text   NOT NULL,
    partition   int    NOT NULL,
    last_offset bigint NOT NULL,
    updated_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (topic, partition)
);
