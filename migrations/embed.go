// Package migrations embeds the forward-only SQL migrations (spec §9.2).
// Files are applied in version order by internal/platform/db; never edit a
// migration that has been released, add a new one instead.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
