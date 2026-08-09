// Package migrations embeds the numbered SQL migration files in this
// directory so internal/store/postgres can apply them without depending on
// a filesystem path at runtime (the binary carries its own migrations).
//
// Files are named NNNN_description.sql. embed.FS.ReadDir returns entries
// sorted by filename, so the numeric prefix is also the apply order.
package migrations

import "embed"

// FS holds every *.sql file in this directory.
//
//go:embed *.sql
var FS embed.FS
