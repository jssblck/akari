// Package migrations embeds the forward-only SQL migration files so the server
// can apply them on startup without shipping the .sql files separately.
package migrations

import "embed"

// FS holds every migration file, named NNNN_description.sql and applied in
// lexical order.
//
//go:embed *.sql
var FS embed.FS
