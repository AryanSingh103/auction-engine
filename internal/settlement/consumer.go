package settlement

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/AryanSingh103/auction-engine/internal/outbox"
)

// Consumer feeds auction_closed events to a Settler, at least once.
type Consumer struct {
	client   *kgo.Client // a consumer group member; it also produces to the DLQ
	settler  *Settler
	dlqTopic string
	retry    time.Duration
	logger   *slog.Logger
}

// NewConsumer returns a consumer. The client must be in a consumer group,
// consume the events topic, and commit only marked offsets
// (kgo.AutoCommitMarks), so an offset is committed only after its event
// was handled. retry is the wait before retrying an event after a
// transient (database) error.
func NewConsumer(client *kgo.Client, settler *Settler, dlqTopic string, retry time.Duration, logger *slog.Logger) *Consumer {
	return &Consumer{client: client, settler: settler, dlqTopic: dlqTopic, retry: retry, logger: logger}
}

// Run handles events until ctx is cancelled. Events are handled one at a
// time in partition order; an event is marked for commit only once it has
// been settled, skipped or dead-lettered. If the process dies first, the
// event is delivered again, and Settle makes that harmless.
func (c *Consumer) Run(ctx context.Context) error {
	for {
		fetches := c.client.PollFetches(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if fetches.IsClientClosed() {
			return errors.New("kafka client closed")
		}
		fetches.EachError(func(topic string, partition int32, err error) {
			c.logger.Error("fetch failed", slog.String("topic", topic), slog.Int("partition", int(partition)), slog.Any("error", err))
		})
		iter := fetches.RecordIter()
		for !iter.Done() {
			r := iter.Next()
			if err := c.handle(ctx, r); err != nil {
				return nil // only a cancelled context stops handling; the rest is redelivered
			}
			c.client.MarkCommitRecords(r)
		}
	}
}

// handle returns an error only when ctx is done.
func (c *Consumer) handle(ctx context.Context, r *kgo.Record) error {
	var ev outbox.Event
	if err := json.Unmarshal(r.Value, &ev); err != nil {
		return c.deadLetter(ctx, r, fmt.Errorf("undecodable event: %w", err))
	}
	if ev.Type != "auction_closed" {
		return nil
	}
	for {
		out, err := c.settler.Settle(ctx, ev.AuctionID)
		switch {
		case err == nil:
			c.logger.Info("auction settled", slog.Int64("auction_id", ev.AuctionID), slog.String("outcome", out.String()))
			return nil
		case ctx.Err() != nil:
			return ctx.Err()
		case errors.Is(err, ErrGaveUp), errors.Is(err, ErrRejected):
			return c.deadLetter(ctx, r, err)
		default:
			// A database error, most likely: not the event's fault, so it
			// is retried rather than dead-lettered. This blocks the
			// partition, deliberately: later events wait, in order.
			c.logger.Error("settlement failed; retrying", slog.Int64("auction_id", ev.AuctionID), slog.Any("error", err))
			if err := sleep(ctx, c.retry); err != nil {
				return err
			}
		}
	}
}

// deadLetter copies the record to the dead-letter topic, with why and
// where it came from in headers, retrying until that succeeds: an event
// must never be dropped silently. Replaying means producing the value back
// to the events topic; Settle makes that safe.
func (c *Consumer) deadLetter(ctx context.Context, r *kgo.Record, cause error) error {
	c.logger.Error("dead-lettering event", slog.String("key", string(r.Key)), slog.Any("error", cause))
	dlq := &kgo.Record{
		Topic: c.dlqTopic,
		Key:   r.Key,
		Value: r.Value,
		Headers: []kgo.RecordHeader{
			{Key: "error", Value: []byte(cause.Error())},
			{Key: "source_topic", Value: []byte(r.Topic)},
			{Key: "source_partition", Value: []byte(strconv.Itoa(int(r.Partition)))},
			{Key: "source_offset", Value: []byte(strconv.FormatInt(r.Offset, 10))},
		},
	}
	for {
		err := c.client.ProduceSync(ctx, dlq).FirstErr()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		c.logger.Error("dead-letter produce failed; retrying", slog.Any("error", err))
		if err := sleep(ctx, c.retry); err != nil {
			return err
		}
	}
}
