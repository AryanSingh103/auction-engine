// Command settler charges the winner of every closed auction exactly once
// (invariant 5, docs/decisions/024). It consumes auction_closed events as a
// consumer group, so any number of copies can run; Kafka splits the
// partitions between them.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/AryanSingh103/auction-engine/internal/breaker"
	"github.com/AryanSingh103/auction-engine/internal/config"
	"github.com/AryanSingh103/auction-engine/internal/kafkaclient"
	"github.com/AryanSingh103/auction-engine/internal/postgres"
	"github.com/AryanSingh103/auction-engine/internal/settlement"
)

const (
	// Settlement handles one event at a time and runs no long
	// transactions: a few connections are plenty.
	dbMaxConns = 4
	// Bounds the final offset commit on shutdown.
	commitTimeout = 5 * time.Second
	// How often to retry creating the dead-letter topic at startup.
	topicRetryInterval = time.Second
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "settler: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.LoadSettler(os.LookupEnv)
	if err != nil {
		return fmt.Errorf("load config:\n%w", err)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))

	// Cancelled on SIGINT or SIGTERM. A settlement in flight is abandoned:
	// its event was not marked, so it is delivered again (to this group,
	// possibly another member), and the same idempotency key makes the
	// retry safe.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := postgres.NewPool(ctx, postgres.PoolConfig{URL: cfg.DatabaseURL, MaxConns: dbMaxConns, IdleInTxTimeout: cfg.DBIdleInTxTimeout})
	if err != nil {
		return err
	}
	defer pool.Close()

	client, err := kafkaclient.New(cfg.KafkaBrokers,
		kgo.ConsumerGroup(cfg.SettlementGroup),
		kgo.ConsumeTopics(cfg.OutboxTopic),
		// A new group starts from the beginning, so events published before
		// the first settler ran are not skipped.
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
		// Commit only what the consumer marked as handled.
		kgo.AutoCommitMarks(),
	)
	if err != nil {
		return err
	}
	defer client.Close()

	for {
		err := kafkaclient.EnsureTopic(ctx, client, cfg.SettlementDLQTopic, 1, cfg.KafkaReplicationFactor)
		if err == nil {
			break
		}
		logger.Warn("dead-letter topic not ready; retrying", slog.Any("error", err))
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(topicRetryInterval):
		}
	}

	b := breaker.New(int(cfg.BreakerFailureThreshold), cfg.BreakerOpenDuration, time.Now, func(from, to breaker.State) {
		logger.Warn("payment circuit breaker changed state", slog.String("from", from.String()), slog.String("to", to.String()))
	})
	settler := settlement.NewSettler(pool, settlement.NewPaymentClient(cfg.PaysimURL, cfg.PaymentTimeout), b,
		int(cfg.PaymentMaxAttempts), cfg.PaymentBackoffBase, cfg.PaymentBackoffCap)
	consumer := settlement.NewConsumer(client, settler, cfg.SettlementDLQTopic, cfg.SettlementRetryInterval, logger)

	logger.Info("settler started", slog.String("topic", cfg.OutboxTopic), slog.String("group", cfg.SettlementGroup))
	runErr := consumer.Run(ctx)

	// Commit what was handled before leaving the group, so the next member
	// does not redo it (harmless, but wasted work).
	commitCtx, cancel := context.WithTimeout(context.Background(), commitTimeout)
	defer cancel()
	if err := client.CommitMarkedOffsets(commitCtx); err != nil {
		logger.Warn("final offset commit failed; those events will be delivered again", slog.Any("error", err))
	}
	logger.Info("settler stopped")
	return runErr
}
