#!/bin/bash
# ClickHouse backups, run inside the clickhouse container on CH_1 by cron
# (any replica would do: they hold the same data):
#   backup.sh full   weekly: full backup to S3 and to the NFS share
#   backup.sh incr   daily: incremental on the latest full
#   backup.sh list   what is there
# Retention: the NFS copies are pruned here (BACKUP_KEEP_DAYS); for S3 use a
# bucket lifecycle rule on the clickhouse/ prefix.
set -euo pipefail
: "${S3_ENDPOINT:?}" "${S3_BUCKET:?}" "${S3_ACCESS_KEY:?}" "${S3_SECRET_KEY:?}"
KEEP=${BACKUP_KEEP_DAYS:-35}
NOW=$(date -u +%Y%m%dT%H%M%SZ)
S3URL="https://${S3_ENDPOINT}/${S3_BUCKET}/clickhouse"
[ "${S3_URI_STYLE:-host}" = host ] && S3URL="https://${S3_BUCKET}.${S3_ENDPOINT}/clickhouse"
q() { clickhouse client --password "${CLICKHOUSE_PASSWORD}" --user sms -q "$1"; }
s3() { echo "S3('$S3URL/$1', '${S3_ACCESS_KEY}', '${S3_SECRET_KEY}')"; }
latest_full() { ls -1 /backup/clickhouse | grep -- '-full$' | sort | tail -1; }
case "${1:-}" in
full)
  q "BACKUP DATABASE sms TO $(s3 "$NOW-full")"
  q "BACKUP DATABASE sms TO Disk('backups', '$NOW-full')"
  find /backup/clickhouse -mindepth 1 -maxdepth 1 -mtime +"$KEEP" -exec rm -rf {} +
  ;;
incr)
  base=$(latest_full)
  [ -n "$base" ] || { echo "no full backup yet: run full" >&2; exit 1; }
  q "BACKUP DATABASE sms TO $(s3 "$NOW-incr") SETTINGS base_backup = $(s3 "$base")"
  q "BACKUP DATABASE sms TO Disk('backups', '$NOW-incr') SETTINGS base_backup = Disk('backups', '$base')"
  ;;
list)
  ls -1 /backup/clickhouse
  q "SELECT name, status, start_time, end_time, formatReadableSize(total_size) FROM system.backups ORDER BY start_time"
  ;;
*) echo "usage: $0 full|incr|list" >&2; exit 2 ;;
esac
