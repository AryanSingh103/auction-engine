package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/AryanSingh103/auction-engine/internal/postgres"
	"github.com/AryanSingh103/auction-engine/internal/testdb"
)

var server *testdb.Server

func TestMain(m *testing.M) {
	ctx := context.Background()
	var err error
	server, err = testdb.Start(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "postgres tests: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	if err := server.Terminate(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "postgres tests: terminate: %v\n", err)
	}
	os.Exit(code)
}

// A transaction whose client stops talking while holding a row lock (the
// API host died mid-bid) must be ended by the server and its lock released,
// instead of blocking every other bid on the row.
func TestIdleInTransactionReleasesLocks(t *testing.T) {
	ctx := t.Context()
	pool, err := postgres.NewPool(ctx, postgres.PoolConfig{
		URL: server.NewDBURL(t), MaxConns: 4, IdleInTxTimeout: 300 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()

	var setting string
	if err := pool.QueryRow(ctx, `SHOW idle_in_transaction_session_timeout`).Scan(&setting); err != nil {
		t.Fatalf("show setting: %v", err)
	}
	if setting != "300ms" {
		t.Fatalf("idle_in_transaction_session_timeout = %q, want 300ms", setting)
	}

	if _, err := pool.Exec(ctx, `INSERT INTO users (name) VALUES ('u')`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	stalled, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = stalled.Rollback(context.Background()) }()
	if _, err := stalled.Exec(ctx, `SELECT 1 FROM users WHERE id = 1 FOR UPDATE`); err != nil {
		t.Fatalf("lock: %v", err)
	}

	// The "client" now goes silent while holding the lock.
	time.Sleep(700 * time.Millisecond)

	// Another session must be able to take the lock immediately.
	if _, err := pool.Exec(ctx, `SELECT 1 FROM users WHERE id = 1 FOR UPDATE NOWAIT`); err != nil {
		t.Fatalf("lock still held after the idle timeout: %v", err)
	}
	// And the stalled transaction must have been terminated by the server.
	_, err = stalled.Exec(ctx, `SELECT 1`)
	var pgErr *pgconn.PgError
	if err == nil {
		t.Fatal("stalled transaction is still usable; the server did not end it")
	}
	if errors.As(err, &pgErr) && pgErr.Code != "25P03" {
		t.Errorf("stalled transaction error = %v, want SQLSTATE 25P03 (idle_in_transaction_session_timeout)", err)
	}
}
