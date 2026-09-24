// Command migrate applies, rolls back, or reports schema migrations.
//
// It runs as a separate one-shot step (a compose service now, an ECS task
// later), never inside the API process: several API instances starting at
// once would race to migrate, and a bad migration would crash-loop the API
// instead of failing one visible job. See docs/decisions/006.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	// Registers the "pgx" driver with database/sql; goose needs *sql.DB.
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/AryanSingh103/auction-engine/internal/config"
	"github.com/AryanSingh103/auction-engine/migrations"
)

const usage = "usage: migrate up|down|status"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "migrate: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) != 1 {
		return errors.New(usage)
	}
	command := args[0]

	cfg, err := config.LoadMigrate(os.LookupEnv)
	if err != nil {
		return fmt.Errorf("load config:\n%w", err)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))

	// Cancelling on SIGTERM lets goose roll back the in-flight migration's
	// transaction rather than being killed mid-statement.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	db, err := sql.Open("pgx", cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	// A Close error at process exit is not actionable: the process is ending
	// and the server drops the session either way.
	defer func() { _ = db.Close() }()

	// migrations.NewProvider takes a Postgres advisory lock, so overlapping
	// migrate jobs (e.g. a redeploy racing a previous one) run one at a time.
	provider, err := migrations.NewProvider(db)
	if err != nil {
		return err
	}

	switch command {
	case "up":
		results, err := provider.Up(ctx)
		for _, r := range results {
			logResult(logger, r)
		}
		if err != nil {
			return fmt.Errorf("migrate up: %w", err)
		}
		logger.Info("migrations up to date", slog.Int("applied", len(results)))
	case "down":
		r, err := provider.Down(ctx)
		if r != nil {
			logResult(logger, r)
		}
		if err != nil {
			return fmt.Errorf("migrate down: %w", err)
		}
	case "status":
		statuses, err := provider.Status(ctx)
		if err != nil {
			return fmt.Errorf("migration status: %w", err)
		}
		for _, s := range statuses {
			attrs := []slog.Attr{
				slog.Int64("version", s.Source.Version),
				slog.String("file", s.Source.Path),
				slog.String("state", string(s.State)),
			}
			if !s.AppliedAt.IsZero() {
				attrs = append(attrs, slog.Time("applied_at", s.AppliedAt))
			}
			logger.LogAttrs(ctx, slog.LevelInfo, "migration", attrs...)
		}
	default:
		return fmt.Errorf("unknown command %q; %s", command, usage)
	}
	return nil
}

func logResult(logger *slog.Logger, r *goose.MigrationResult) {
	level := slog.LevelInfo
	if r.Error != nil {
		level = slog.LevelError
	}
	logger.LogAttrs(context.Background(), level, "migration "+r.Direction,
		slog.Int64("version", r.Source.Version),
		slog.String("file", r.Source.Path),
		slog.Float64("duration_ms", float64(r.Duration.Microseconds())/1000),
		slog.Any("error", r.Error),
	)
}
