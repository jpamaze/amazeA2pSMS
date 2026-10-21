# PostgreSQL 16 with Patroni (failover) and pgBackRest (backups, WAL
# archiving). The postgres image already carries the PGDG apt repository.
FROM postgres:16
RUN apt-get update \
 && apt-get install -y --no-install-recommends pgbackrest python3-venv curl gettext-base \
 && python3 -m venv /opt/patroni \
 && /opt/patroni/bin/pip install --no-cache-dir "patroni[etcd3,psycopg3]==4.0.4" \
 && rm -rf /var/lib/apt/lists/* \
 && mkdir -p /etc/amaze /var/run/postgresql /var/lib/pgbackrest /var/log/pgbackrest \
 && chown -R postgres:postgres /etc/amaze /var/run/postgresql /var/lib/pgbackrest /var/log/pgbackrest
COPY --chown=postgres:postgres patroni.yml.tmpl pgbackrest.conf.tmpl entrypoint.sh backup.sh /etc/amaze/
ENV PATH=/opt/patroni/bin:$PATH
USER postgres
ENTRYPOINT ["/etc/amaze/entrypoint.sh"]
