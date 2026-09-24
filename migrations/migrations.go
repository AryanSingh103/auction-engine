// Package migrations embeds the SQL schema migrations applied by goose.
//
// Files are named NNNNN_description.sql and applied in version order. Each
// has an Up and a Down section; goose runs each migration in its own
// transaction, so a failing migration leaves no partial schema behind.
package migrations

import "embed"

// FS holds every migration file, compiled into the binary so the migrate
// command needs nothing on disk at runtime.
//
//go:embed *.sql
var FS embed.FS
