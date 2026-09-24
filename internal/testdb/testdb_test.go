package testdb

import (
	"context"
	"fmt"
	"os"
	"testing"
)

var server *Server

func TestMain(m *testing.M) {
	ctx := context.Background()
	var err error
	server, err = Start(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "testdb: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	if err := server.Terminate(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "testdb: terminate: %v\n", err)
	}
	os.Exit(code)
}

func TestNewDBIsMigrated(t *testing.T) {
	pool := server.NewDB(t, 2)

	var version int64
	if err := pool.QueryRow(t.Context(), "SELECT max(version_id) FROM goose_db_version").Scan(&version); err != nil {
		t.Fatalf("read migration version: %v", err)
	}
	if version < 1 {
		t.Errorf("migration version = %d, want >= 1", version)
	}
}

func TestNewDBIsIsolated(t *testing.T) {
	a := server.NewDB(t, 2)
	b := server.NewDB(t, 2)

	if _, err := a.Exec(t.Context(), "INSERT INTO users (name) VALUES ('only in a')"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	var n int
	if err := b.QueryRow(t.Context(), "SELECT count(*) FROM users").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("database b sees %d users written to database a, want 0", n)
	}
}
