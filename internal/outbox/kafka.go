package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/twmb/franz-go/pkg/kgo"
)

// KafkaPublisher publishes events to one topic, keyed by auction id so
// each auction's events land on one partition, in order.
//
// The client must be idempotent (franz-go's default) with acks from all
// in-sync replicas (also the default): then a retried produce cannot write
// a record twice or reorder a partition. It should also set
// kgo.RecordDeliveryTimeout, because context cancellation cannot fail a
// batch that has already been sent: the client cannot know whether the
// broker wrote it, and failing it would break the idempotent sequence.
// So a Publish may outlast its context; the relay's safety does not depend
// on it returning in time (see Relay.PublishBatch).
type KafkaPublisher struct {
	client *kgo.Client
	topic  string
}

// NewKafkaPublisher returns a publisher to topic.
func NewKafkaPublisher(client *kgo.Client, topic string) *KafkaPublisher {
	return &KafkaPublisher{client: client, topic: topic}
}

// Publish produces the events in order and waits for every acknowledgement.
func (p *KafkaPublisher) Publish(ctx context.Context, events []Event) error {
	records := make([]*kgo.Record, len(events))
	for i, e := range events {
		value, err := json.Marshal(e)
		if err != nil {
			return fmt.Errorf("encode event %d: %w", e.ID, err)
		}
		records[i] = &kgo.Record{
			Topic: p.topic,
			Key:   strconv.AppendInt(nil, e.AuctionID, 10),
			Value: value,
		}
	}
	return p.client.ProduceSync(ctx, records...).FirstErr()
}
