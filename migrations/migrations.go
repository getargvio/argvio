// Package migrations embeds the SQL migration files so `argvio migrate` can run
// them without needing the golang-migrate CLI or filesystem access to the
// repo at runtime (e.g. from inside a built container image).
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
