// Package migrations embeds the SQL schema so the binary that runs a
// migration is always the binary that was built against it.
package migrations

import "embed"

// FS holds every migration, applied in filename order.
//
//go:embed *.sql
var FS embed.FS
