// Package kafkaclient builds franz-go clients with this project's settings
// and creates topics, like internal/redisclient does for Redis.
package kafkaclient

import (
	"context"
	"errors"
	"fmt"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"
)

// New returns a client for brokers. Producing is idempotent with acks from
// all in-sync replicas; both are franz-go defaults, stated here so a
// future option cannot silently turn them off (a retried produce could
// then duplicate or reorder records within a partition).
func New(brokers []string, opts ...kgo.Opt) (*kgo.Client, error) {
	base := []kgo.Opt{
		kgo.SeedBrokers(brokers...),
		kgo.RequiredAcks(kgo.AllISRAcks()),
	}
	c, err := kgo.NewClient(append(base, opts...)...)
	if err != nil {
		return nil, fmt.Errorf("kafka client: %w", err)
	}
	return c, nil
}

// EnsureTopic creates topic if it does not exist. An existing topic is
// left alone, even if its partition count differs: repartitioning would
// move keys between partitions and break per-key order, so it must be a
// deliberate operation, never a side effect of a deploy.
func EnsureTopic(ctx context.Context, c *kgo.Client, topic string, partitions int32, replication int16) error {
	// For a single topic, kadm returns the broker's per-topic error as err
	// as well as in resp.Err.
	resp, err := kadm.NewClient(c).CreateTopic(ctx, partitions, replication, nil, topic)
	if err == nil {
		err = resp.Err
	}
	if err != nil && !errors.Is(err, kerr.TopicAlreadyExists) {
		return fmt.Errorf("create topic %s: %w", topic, err)
	}
	return nil
}
