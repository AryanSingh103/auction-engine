// Package testdb gives integration tests a real, migrated Postgres.
//
// One container is started per test binary (from TestMain). The migrations
// are applied once, into a template database, and every test then gets its
// own database cloned from that template with CREATE DATABASE ... TEMPLATE.
// Cloning is a file copy inside Postgres (milliseconds), so tests are
// isolated from each other and can run in parallel without re-migrating.
//
// There are deliberately no database mocks in this project: the behavior
// under test (row locks, constraints, triggers, isolation) only exists in a
// real Postgres.
package testdb

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // "pgx" driver for goose's *sql.DB
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/AryanSingh103/auction-engine/migrations"
)

// Image must match the postgres service in compose.yaml, so tests run
// against the same server version as local development.
const Image = "postgres:18.6-alpine3.24"

const templateDB = "migrated_template"

// Server is a running Postgres container with a migrated template database.
type Server struct {
	container *tcpostgres.PostgresContainer
	// adminURL connects to the maintenance database, used to create and
	// drop per-test databases.
	adminURL string
	counter  atomic.Int64
}

// Start launches the container and applies all migrations to the template
// database. Call it once from TestMain and Terminate it when tests finish.
func Start(ctx context.Context) (*Server, error) {
	c, err := tcpostgres.Run(ctx, Image,
		tcpostgres.WithDatabase("postgres"),
		tcpostgres.WithUsername("test"),
		tcpostgres.WithPassword("test"),
		// Waits for the "ready to accept connections" log line twice (the
		// image restarts Postgres after initdb) and for the port, so we
		// never connect to the temporary init server.
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		return nil, fmt.Errorf("start postgres container: %w", err)
	}
	s := &Server{container: c}

	s.adminURL, err = c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return nil, s.fail(ctx, fmt.Errorf("connection string: %w", err))
	}
	if err := s.exec(ctx, "CREATE DATABASE "+templateDB); err != nil {
		return nil, s.fail(ctx, err)
	}
	if err := migrate(ctx, s.urlFor(templateDB)); err != nil {
		return nil, s.fail(ctx, err)
	}
	return s, nil
}

// Terminate stops and removes the container.
func (s *Server) Terminate(ctx context.Context) error {
	return testcontainers.TerminateContainer(s.container, testcontainers.StopContext(ctx))
}

// NewDB creates a fresh, fully migrated database for one test and returns a
// pool connected to it with at most maxConns connections. The pool is closed
// and the database dropped when the test finishes.
func (s *Server) NewDB(t testing.TB, maxConns int32) *pgxpool.Pool {
	t.Helper()
	ctx := t.Context()

	name := fmt.Sprintf("test_%d", s.counter.Add(1))
	if err := s.exec(ctx, fmt.Sprintf("CREATE DATABASE %s TEMPLATE %s", name, templateDB)); err != nil {
		t.Fatalf("create test database: %v", err)
	}

	cfg, err := pgxpool.ParseConfig(s.urlFor(name))
	if err != nil {
		t.Fatalf("parse pool config: %v", err)
	}
	cfg.MaxConns = maxConns
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("create pool: %v", err)
	}

	t.Cleanup(func() {
		pool.Close()
		// t.Context() is already cancelled when cleanups run.
		if err := s.exec(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)"); err != nil {
			t.Errorf("drop test database %s: %v", name, err)
		}
	})
	return pool
}

// urlFor returns the admin connection URL pointed at database name.
func (s *Server) urlFor(name string) string {
	// adminURL ends in "/postgres?sslmode=disable".
	return strings.Replace(s.adminURL, "/postgres?", "/"+name+"?", 1)
}

func (s *Server) exec(ctx context.Context, sql string) error {
	conn, err := pgx.Connect(ctx, s.adminURL)
	if err != nil {
		return fmt.Errorf("connect admin: %w", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	if _, err := conn.Exec(ctx, sql); err != nil {
		return fmt.Errorf("%s: %w", sql, err)
	}
	return nil
}

// fail terminates the container after a setup error so it does not leak,
// and returns the original error.
func (s *Server) fail(ctx context.Context, err error) error {
	_ = s.Terminate(ctx)
	return err
}

func migrate(ctx context.Context, url string) error {
	db, err := sql.Open("pgx", url)
	if err != nil {
		return fmt.Errorf("open template database: %w", err)
	}
	// The template must have no open connections when it is cloned, so this
	// handle is closed before Start returns.
	defer func() { _ = db.Close() }()

	p, err := migrations.NewProvider(db)
	if err != nil {
		return err
	}
	if _, err := p.Up(ctx); err != nil {
		return fmt.Errorf("migrate template database: %w", err)
	}
	return nil
}
