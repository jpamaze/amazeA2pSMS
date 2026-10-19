// Package migrations embeds the SQL migrations so cmd/migrate ships them in its binary.
package migrations

import "embed"

// Postgres holds postgres/NNNN_name.sql, applied in file-name order.
//
//go:embed postgres/*.sql
var Postgres embed.FS
