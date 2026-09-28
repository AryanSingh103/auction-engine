// Command relay publishes the transactional outbox to Kafka
// (docs/decisions/022). Any number of copies can run: one batch is
// published at a time, by whichever copy gets the lock, so a dead copy
// needs no failover.
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

	"github.com/AryanSingh103/auction-engine/internal/config"
	"github.com/AryanSingh103/auction-engine/internal/kafkaclient"
	"github.com/AryanSingh103/auction-engine/internal/outbox"
	"github.com/AryanSingh103/auction-engine/internal/postgres"
)

// The relay runs one transaction at a time, so it needs one connection; the
// second lets a slow close of the first never block the next batch.
const dbMaxConns = 2

// How often to retry creating the topic while the broker is unreachable.
const topicRetryInterval = time.Second

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "relay: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.LoadRelay(os.LookupEnv)
	if err != nil {
		return fmt.Errorf("load config:\n%w", err)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))

	// Cancelled on SIGINT or SIGTERM. A batch in flight is then abandoned:
	// its transaction rolls back and the events are published again by the
	// next relay, which is safe because delivery is at least once anyway.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := postgres.NewPool(ctx, postgres.PoolConfig{
		URL:             cfg.DatabaseURL,
		MaxConns:        dbMaxConns,
		IdleInTxTimeout: cfg.DBIdleInTxTimeout,
	})
	if err != nil {
		return err
	}
	defer pool.Close()

	// A record that has not been sent within the publish timeout is failed;
	// one already sent can only finish or fail with its connection (see
	// outbox.KafkaPublisher).
	client, err := kafkaclient.New(cfg.KafkaBrokers, kgo.RecordDeliveryTimeout(cfg.RelayPublishTimeout))
	if err != nil {
		return err
	}
	defer client.Close()

	// Retried rather than fatal, so the relay can start before the broker.
	for {
		err := kafkaclient.EnsureTopic(ctx, client, cfg.OutboxTopic, cfg.OutboxTopicPartitions, cfg.KafkaReplicationFactor)
		if err == nil {
			break
		}
		logger.Warn("outbox topic not ready; retrying", slog.Any("error", err))
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(topicRetryInterval):
		}
	}

	logger.Info("relay started",
		slog.String("topic", cfg.OutboxTopic),
		slog.Int("batch_size", int(cfg.RelayBatchSize)))
	relay := outbox.NewRelay(pool, outbox.NewKafkaPublisher(client, cfg.OutboxTopic), int(cfg.RelayBatchSize), cfg.RelayPublishTimeout, nil)
	relay.Run(ctx, cfg.RelayPollInterval, logger)
	logger.Info("relay stopped")
	return nil
}
