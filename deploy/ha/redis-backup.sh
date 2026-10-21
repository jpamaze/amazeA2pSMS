#!/bin/bash
# Redis snapshot backup, from cron on each Redis VM (only a replica acts, so
# the primary never forks under load). Copies dump.rdb to the NFS share and
# to S3. Restoring Redis is rarely needed: wallets are rebuilt from
# PostgreSQL by wallet-resync; the snapshot keeps in-flight message state
# (receipt matching) and idempotency keys.
#   deploy/ha/redis-backup.sh        (run from the repository checkout)
set -euo pipefail
cd "$(dirname "$0")"
set -a; . ./.env; set +a
dc() { docker compose -f redis.yml --env-file .env "$@"; }
cli() { dc exec -T redis redis-cli -a "$REDIS_PASSWORD" --no-auth-warning "$@"; }
cli info replication | grep -q '^role:slave' || exit 0
before=$(cli LASTSAVE | tr -d '\r')
cli BGSAVE >/dev/null
for _ in $(seq 1 600); do [ "$(cli LASTSAVE | tr -d '\r')" != "$before" ] && break; sleep 1; done
name="redis-$(hostname)-$(date -u +%Y%m%dT%H%M%SZ).rdb"
mkdir -p "$BACKUP_NFS_PATH/redis"
dc cp redis:/data/dump.rdb "$BACKUP_NFS_PATH/redis/$name"
docker run --rm -v "$BACKUP_NFS_PATH/redis:/data:ro" \
  -e RCLONE_CONFIG_S3_TYPE=s3 -e RCLONE_CONFIG_S3_PROVIDER=Other -e RCLONE_CONFIG_S3_ENDPOINT="https://$S3_ENDPOINT" \
  -e RCLONE_CONFIG_S3_ACCESS_KEY_ID="$S3_ACCESS_KEY" -e RCLONE_CONFIG_S3_SECRET_ACCESS_KEY="$S3_SECRET_KEY" \
  -e RCLONE_CONFIG_S3_REGION="$S3_REGION" -e RCLONE_CONFIG_S3_FORCE_PATH_STYLE="$([ "${S3_URI_STYLE:-host}" = path ] && echo true || echo false)" \
  rclone/rclone:1.68 copyto "/data/$name" "s3:$S3_BUCKET/redis/$name"
find "$BACKUP_NFS_PATH/redis" -name 'redis-*.rdb' -mtime +"${BACKUP_KEEP_DAYS:-35}" -delete
echo "saved $name"
