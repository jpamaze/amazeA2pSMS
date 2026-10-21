// Package migrations embeds the SQL migrations so cmd/migrate ships them in its binary.
package migrations

import "embed"

// Postgres holds postgres/NNNN_name.sql, applied in file-name order.
//
//go:embed postgres/*.sql
var Postgres embed.FS

// ClickHouse holds clickhouse/*.sql (idempotent DDL), applied by ch-writer.
//
//go:embed clickhouse/*.sql
var ClickHouse embed.FS
