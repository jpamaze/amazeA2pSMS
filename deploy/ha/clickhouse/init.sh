#!/bin/bash
# Run by the clickhouse image on first start (docker-entrypoint-initdb.d):
# the sms database is Replicated, so tables the ch-writer creates on any
# replica are created, as replicated tables, on all three.
set -e
clickhouse client -n <<SQL
CREATE DATABASE IF NOT EXISTS sms ENGINE = Replicated('/clickhouse/databases/sms', '{shard}', '{replica}');
SQL
