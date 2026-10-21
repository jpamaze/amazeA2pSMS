#!/bin/bash
# PostgreSQL backups with pgBackRest, run inside the patroni container:
#   backup.sh init        once, on the leader: create the stanza in both repos
#   backup.sh full|diff   from cron on every PostgreSQL VM; only the current
#                         leader backs up (to S3, then to the NFS share)
#   backup.sh check       archive and repositories are healthy
#   backup.sh info        what is in the repositories
# post_bootstrap (Patroni) calls `bootstrap`: it creates the application
# role and database on a new cluster.
set -euo pipefail
PGBR="pgbackrest --config=/etc/amaze/pgbackrest.conf --stanza=amaze"
leader() { curl -sf -o /dev/null http://127.0.0.1:8008/primary; }
case "${1:-}" in
bootstrap)
  psql -v ON_ERROR_STOP=1 -U postgres -h /var/run/postgresql <<SQL
CREATE ROLE sms LOGIN PASSWORD '${APP_DB_PASSWORD}';
CREATE DATABASE sms OWNER sms;
SQL
  ;;
init)
  leader || { echo "run init on the leader" >&2; exit 1; }
  $PGBR stanza-create
  touch /backup/pgbackrest/stanza.ok
  $PGBR check
  $PGBR --type=full --repo=1 backup
  $PGBR --type=full --repo=2 backup
  ;;
full|diff)
  leader || exit 0
  for repo in 1 2; do $PGBR --type="$1" --repo=$repo backup; done
  $PGBR expire
  ;;
check) $PGBR check ;;
info) $PGBR info ;;
*) echo "usage: $0 init|full|diff|check|info" >&2; exit 2 ;;
esac
