// Package migrations embeds the SQL schema migrations applied by goose.
//
// Files are named NNNNN_description.sql and applied in version order. Each
// has an Up and a Down section; goose runs each migration in its own
// transaction, so a failing migration leaves no partial schema behind.
package migrations

import (
	"database/sql"
	"embed"
	"fmt"

	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

// FS holds every migration file, compiled into the binary so the migrate
// command needs nothing on disk at runtime.
//
//go:embed *.sql
var FS embed.FS

// NewProvider returns a goose provider for these migrations on db, guarded
// by a Postgres session-level advisory lock: if two migration runs start at
// once, the second waits instead of applying the same migrations
// concurrently. Shared by cmd/migrate and the test harness so tests build
// their schema exactly the way deployments do.
func NewProvider(db *sql.DB) (*goose.Provider, error) {
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return nil, fmt.Errorf("create migration lock: %w", err)
	}
	p, err := goose.NewProvider(goose.DialectPostgres, db, FS, goose.WithSessionLocker(locker))
	if err != nil {
		return nil, fmt.Errorf("create migration provider: %w", err)
	}
	return p, nil
}
