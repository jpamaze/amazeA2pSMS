#!/usr/bin/env bash
# Creates the platform topics. Names must match internal/platform/kafka.
# Usage: BROKERS=host:9092 VENDORS="sim vendor-a" REPLICAS=1 ./kafka-topics.sh
set -euo pipefail

BROKERS=${BROKERS:-localhost:19092}
REPLICAS=${REPLICAS:-1}      # 3 in production
PARTITIONS=${PARTITIONS:-12}
VENDORS=${VENDORS:-sim}

create() { # name partitions retention_ms
  rpk topic create "$1" -X brokers="$BROKERS" -p "$2" -r "$REPLICAS" \
    -c retention.ms="$3" -c compression.type=zstd -c min.insync.replicas=$(( REPLICAS > 1 ? 2 : 1 )) \
    || echo "topic $1 exists"
}

H72=$((72 * 3600 * 1000))
D7=$((7 * 24 * 3600 * 1000))

create sms.submit          "$PARTITIONS" "$H72"
create sms.status          "$PARTITIONS" "$H72"
create sms.dlr             "$PARTITIONS" "$H72"
create sms.mo              "$PARTITIONS" "$H72"
create sms.billing.events  "$PARTITIONS" "$D7"
create sms.config.changes  1             "$D7"
create sms.webhooks        "$PARTITIONS" "$H72"
create sms.records         "$PARTITIONS" "$H72"
for i in 1 2 3 4; do create "sms.webhooks.retry.$i" "$PARTITIONS" "$D7"; done
create sms.webhooks.dead   1             "$D7"
for v in $VENDORS; do
  create "sms.route.$v" "$PARTITIONS" "$H72"
done
