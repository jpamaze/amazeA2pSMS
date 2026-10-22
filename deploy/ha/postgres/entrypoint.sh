#!/bin/bash
# Renders the Patroni and pgBackRest configuration from the environment and
# starts Patroni, which runs PostgreSQL.
set -euo pipefail
: "${NODE_NAME:?}" "${SELF:?}" "${PG_1:?}" "${PG_2:?}" "${PG_3:?}" "${POSTGRES_PASSWORD:?}" "${REPLICATION_PASSWORD:?}"
: "${PATRONI_REST_PASSWORD:?}" "${ETCD_ROOT_PASSWORD:?}"
export S3_URI_STYLE=${S3_URI_STYLE:-host} S3_VERIFY_TLS=${S3_VERIFY_TLS:-y} S3_PORT=${S3_PORT:-443}
export BACKUP_RETENTION_FULL_S3=${BACKUP_RETENTION_FULL_S3:-4} BACKUP_RETENTION_FULL_NFS=${BACKUP_RETENTION_FULL_NFS:-2}
envsubst <"/etc/amaze/patroni.yml.tmpl" >/etc/amaze/patroni.yml
envsubst <"/etc/amaze/pgbackrest.conf.tmpl" >/etc/amaze/pgbackrest.conf
chmod 600 /etc/amaze/patroni.yml /etc/amaze/pgbackrest.conf
exec patroni /etc/amaze/patroni.yml
