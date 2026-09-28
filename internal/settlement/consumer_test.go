package settlement_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/AryanSingh103/auction-engine/internal/breaker"
	"github.com/AryanSingh103/auction-engine/internal/outbox"
	"github.com/AryanSingh103/auction-engine/internal/settlement"
)

// pipeline runs the relay and a settlement consumer until the test ends.
type pipeline struct {
	topic, dlq string
	cancel     context.CancelFunc
	done       sync.WaitGroup
}

func startPipeline(t *testing.T, pool *pgxpool.Pool, settler *settlement.Settler) *pipeline {
	t.Helper()
	p := &pipeline{topic: kafka.NewTopic(t, 3), dlq: kafka.NewTopic(t, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel

	relay := outbox.NewRelay(pool, outbox.NewKafkaPublisher(kafka.NewClient(t, kgo.RecordDeliveryTimeout(5*time.Second)), p.topic), 50, 5*time.Second)
	p.done.Go(func() { relay.Run(ctx, 10*time.Millisecond, discard) })

	group := kafka.NewClient(t,
		kgo.ConsumerGroup(p.topic+"-settlement"),
		kgo.ConsumeTopics(p.topic),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
		kgo.AutoCommitMarks(),
	)
	consumer := settlement.NewConsumer(group, settler, p.dlq, 10*time.Millisecond, discard)
	p.done.Go(func() { _ = consumer.Run(ctx) })

	t.Cleanup(func() { p.cancel(); p.done.Wait() })
	return p
}

// waitFor polls cond until it holds or 20s pass.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func settled(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM invoices WHERE status <> 'pending'`).Scan(&n); err != nil {
		t.Fatalf("count settled: %v", err)
	}
	return n
}

func TestPipelineSettlesClosedAuctions(t *testing.T) {
	pool := db.NewDB(t, 20)
	paid := []int64{closedAuction(t, pool, 1100), closedAuction(t, pool, 2200), closedAuction(t, pool, 3300)}
	declined := closedAuction(t, pool, 4413)
	noWinner := closedAuction(t, pool, 0)

	// A provider that fails, loses responses and hangs along the way.
	url := provider(t, pool, 300*time.Millisecond, failBefore, lostAfter, hangAfter, lostAfter)
	p := startPipeline(t, pool, newSettler(pool, url, 10))

	waitFor(t, "four settled invoices", func() bool { return settled(t, pool) == 4 })
	for _, a := range paid {
		wantPaidOnce(t, books(t, pool, a))
	}
	if l := books(t, pool, declined); l.status != "failed" || l.charges != 0 {
		t.Errorf("declined auction: books = %+v", l)
	}
	if l := books(t, pool, noWinner); l.invoices != 0 {
		t.Errorf("auction without bids: books = %+v", l)
	}
	if got := kafka.ReadAll(t, p.dlq, 1, time.Second); len(got) != 0 {
		t.Errorf("%d dead-lettered events, want 0", len(got))
	}
}

// The same event delivered again (a relay republish, a rebalance) changes
// nothing.
func TestPipelineIgnoresRedeliveries(t *testing.T) {
	pool := db.NewDB(t, 20)
	a := closedAuction(t, pool, 1100)
	p := startPipeline(t, pool, newSettler(pool, provider(t, pool, 0), 3))
	waitFor(t, "the invoice to be paid", func() bool { return settled(t, pool) == 1 })

	events := kafka.ReadAll(t, p.topic, 3, 10*time.Second) // two bids and the close
	producer := kafka.NewClient(t)
	for range 3 {
		for _, r := range events {
			dup := &kgo.Record{Topic: p.topic, Key: r.Key, Value: r.Value}
			if err := producer.ProduceSync(t.Context(), dup).FirstErr(); err != nil {
				t.Fatalf("produce duplicate: %v", err)
			}
		}
	}
	// Wait until the consumer group has processed the duplicates too.
	waitFor(t, "duplicates to be consumed", func() bool { return committedOffsets(t, p.topic) >= int64(len(events)*4) })
	wantPaidOnce(t, books(t, pool, a))
}

// committedOffsets sums the settlement group's committed offsets on topic
// (0 if none are committed yet).
func committedOffsets(t *testing.T, topic string) int64 {
	t.Helper()
	offsets, err := kadm.NewClient(kafka.NewClient(t)).FetchOffsets(t.Context(), topic+"-settlement")
	if err != nil {
		return 0
	}
	var sum int64
	offsets.Each(func(o kadm.OffsetResponse) {
		if o.Topic == topic && o.Err == nil {
			sum += o.At
		}
	})
	return sum
}

func TestPipelineDeadLettersWhenTheOutcomeStaysUnknown(t *testing.T) {
	pool := db.NewDB(t, 20)
	a := closedAuction(t, pool, 1100)
	url := provider(t, pool, 0, lostAfter, lostAfter)
	b := breaker.New(1000, time.Second, time.Now, nil)
	s := settlement.NewSettler(pool, settlement.NewPaymentClient(url, 200*time.Millisecond), b, 2, time.Millisecond, 2*time.Millisecond)
	p := startPipeline(t, pool, s)

	dead := kafka.ReadAll(t, p.dlq, 1, 20*time.Second)
	if len(dead) != 1 {
		t.Fatalf("%d dead-lettered events, want 1", len(dead))
	}
	headers := map[string]string{}
	for _, h := range dead[0].Headers {
		headers[h.Key] = string(h.Value)
	}
	if headers["source_topic"] != p.topic || headers["error"] == "" {
		t.Errorf("dead-letter headers = %v", headers)
	}
	// Pending, not failed: a charge exists (both responses were lost).
	if l := books(t, pool, a); l.status != "pending" || l.charges != 1 {
		t.Fatalf("books = %+v; want a pending invoice with its one charge", l)
	}
}

func TestPipelineDeadLettersPoisonMessages(t *testing.T) {
	pool := db.NewDB(t, 20)
	p := startPipeline(t, pool, newSettler(pool, provider(t, pool, 0), 3))
	producer := kafka.NewClient(t)
	if err := producer.ProduceSync(t.Context(), &kgo.Record{Topic: p.topic, Key: []byte("1"), Value: []byte("not json")}).FirstErr(); err != nil {
		t.Fatalf("produce: %v", err)
	}
	dead := kafka.ReadAll(t, p.dlq, 1, 20*time.Second)
	if len(dead) != 1 || string(dead[0].Value) != "not json" {
		t.Fatalf("dead letters = %v, want the poison message", dead)
	}
}
