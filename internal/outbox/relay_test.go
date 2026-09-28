package outbox_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/AryanSingh103/auction-engine/internal/auction"
	"github.com/AryanSingh103/auction-engine/internal/outbox"
	"github.com/AryanSingh103/auction-engine/internal/testdb"
	"github.com/AryanSingh103/auction-engine/internal/testkafka"
)

var (
	db    *testdb.Server
	kafka *testkafka.Server
)

func TestMain(m *testing.M) {
	ctx := context.Background()
	var err error
	if db, err = testdb.Start(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "outbox tests: %v\n", err)
		os.Exit(1)
	}
	if kafka, err = testkafka.Start(ctx); err != nil {
		_ = db.Terminate(ctx)
		fmt.Fprintf(os.Stderr, "outbox tests: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	_ = db.Terminate(ctx)
	_ = kafka.Terminate(ctx)
	os.Exit(code)
}

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

// seed creates users 1..users and n open auctions, returning their ids.
func seed(t *testing.T, pool *pgxpool.Pool, users, n int) []int64 {
	t.Helper()
	ctx := t.Context()
	if _, err := pool.Exec(ctx, `INSERT INTO users (name) SELECT 'user ' || g FROM generate_series(1, $1) g`, users); err != nil {
		t.Fatalf("seed users: %v", err)
	}
	ids := make([]int64, n)
	for i := range ids {
		err := pool.QueryRow(ctx, `
			WITH item AS (INSERT INTO items (title) VALUES ('unit') RETURNING id)
			INSERT INTO auctions (item_id, start_at, end_at, starting_price, min_increment)
			SELECT id, clock_timestamp() - interval '1 minute', clock_timestamp() + interval '1 hour', 100, 1 FROM item
			RETURNING id`).Scan(&ids[i])
		if err != nil {
			t.Fatalf("seed auction: %v", err)
		}
	}
	return ids
}

// bid places one accepted bid, which writes one outbox event.
func bid(t *testing.T, svc *auction.Service, auctionID, user, amount int64) {
	t.Helper()
	_, _, err := svc.PlaceBid(t.Context(), auction.PlaceBidRequest{
		AuctionID: auctionID, UserID: user, Amount: amount,
		IdempotencyKey: fmt.Sprintf("a%d-u%d-%d", auctionID, user, amount),
	})
	if err != nil {
		t.Fatalf("bid %d on auction %d: %v", amount, auctionID, err)
	}
}

func newPublisher(t *testing.T, topic string) *outbox.KafkaPublisher {
	return outbox.NewKafkaPublisher(kafka.NewClient(t, kgo.RecordDeliveryTimeout(5*time.Second)), topic)
}

func decode(t *testing.T, records []*kgo.Record) []outbox.Event {
	t.Helper()
	out := make([]outbox.Event, len(records))
	for i, r := range records {
		if err := json.Unmarshal(r.Value, &out[i]); err != nil {
			t.Fatalf("decode record: %v", err)
		}
		if key := strconv.FormatInt(out[i].AuctionID, 10); string(r.Key) != key {
			t.Errorf("event %d has key %q, want auction id %s", out[i].ID, r.Key, key)
		}
	}
	return out
}

func unpublished(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM outbox WHERE published_at IS NULL`).Scan(&n); err != nil {
		t.Fatalf("count unpublished: %v", err)
	}
	return n
}

func TestPublishBatchPublishesAndMarks(t *testing.T) {
	pool := db.NewDB(t, 4)
	auctions := seed(t, pool, 2, 1)
	svc := auction.NewService(pool)
	bid(t, svc, auctions[0], 1, 100)
	bid(t, svc, auctions[0], 2, 101)
	bid(t, svc, auctions[0], 1, 102)

	topic := kafka.NewTopic(t, 3)
	relay := outbox.NewRelay(pool, newPublisher(t, topic), 2, 5*time.Second, nil)

	n, held, err := relay.PublishBatch(t.Context())
	if err != nil || !held || n != 2 {
		t.Fatalf("first batch = %d, held %v, err %v; want 2, true, nil", n, held, err)
	}
	if got := unpublished(t, pool); got != 1 {
		t.Fatalf("%d unpublished after a batch of 2 out of 3, want 1", got)
	}
	if n, _, err := relay.PublishBatch(t.Context()); err != nil || n != 1 {
		t.Fatalf("second batch = %d, err %v; want 1, nil", n, err)
	}
	if n, _, err := relay.PublishBatch(t.Context()); err != nil || n != 0 {
		t.Fatalf("empty batch = %d, err %v; want 0, nil", n, err)
	}

	events := decode(t, kafka.ReadAll(t, topic, 3, 10*time.Second))
	if len(events) != 3 {
		t.Fatalf("read %d events, want 3", len(events))
	}
	for i, e := range events {
		if e.Type != "bid_placed" || e.AuctionID != auctions[0] {
			t.Errorf("event %d = %+v", i, e)
		}
		var b auction.Bid
		if err := json.Unmarshal(e.Payload, &b); err != nil || b.Amount != int64(100+i) {
			t.Errorf("event %d payload = %s (%v), want the bid of %d", i, e.Payload, err, 100+i)
		}
	}
}

// While another transaction holds the relay lock, a relay does nothing.
func TestPublishBatchStandsByWhileAnotherRelayIsActive(t *testing.T) {
	pool := db.NewDB(t, 4)
	auctions := seed(t, pool, 1, 1)
	bid(t, auction.NewService(pool), auctions[0], 1, 100)

	other, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = other.Rollback(context.Background()) }()
	if _, err := other.Exec(t.Context(), `SELECT pg_advisory_xact_lock($1)`, outbox.LockKey); err != nil {
		t.Fatalf("take lock: %v", err)
	}

	relay := outbox.NewRelay(pool, newPublisher(t, kafka.NewTopic(t, 1)), 10, 5*time.Second, nil)
	n, held, err := relay.PublishBatch(t.Context())
	if err != nil || held || n != 0 {
		t.Fatalf("batch while locked = %d, held %v, err %v; want 0, false, nil", n, held, err)
	}
	if got := unpublished(t, pool); got != 1 {
		t.Fatalf("%d unpublished, want 1", got)
	}
}

type failingPublisher struct{ inner outbox.Publisher }

// Publish delivers through inner (if set) and then reports failure, like a
// relay that published but crashed or timed out before it could commit.
func (p failingPublisher) Publish(ctx context.Context, events []outbox.Event) error {
	if p.inner != nil {
		if err := p.inner.Publish(ctx, events); err != nil {
			return err
		}
	}
	return errors.New("injected failure")
}

// A failed publish leaves the events unpublished, and the next batch
// starts from the same row: no event is skipped.
func TestFailedPublishIsRetriedFromTheSameRow(t *testing.T) {
	pool := db.NewDB(t, 4)
	auctions := seed(t, pool, 2, 1)
	svc := auction.NewService(pool)
	bid(t, svc, auctions[0], 1, 100)
	bid(t, svc, auctions[0], 2, 101)

	if _, _, err := outbox.NewRelay(pool, failingPublisher{}, 10, 5*time.Second, nil).PublishBatch(t.Context()); err == nil {
		t.Fatal("PublishBatch with a failing publisher returned nil")
	}
	if got := unpublished(t, pool); got != 2 {
		t.Fatalf("%d unpublished after a failed publish, want 2", got)
	}

	topic := kafka.NewTopic(t, 1)
	if n, _, err := outbox.NewRelay(pool, newPublisher(t, topic), 10, 5*time.Second, nil).PublishBatch(t.Context()); err != nil || n != 2 {
		t.Fatalf("retry = %d, err %v; want 2, nil", n, err)
	}
	if got := len(kafka.ReadAll(t, topic, 2, 10*time.Second)); got != 2 {
		t.Fatalf("read %d events, want 2", got)
	}
}

// Published but not marked (the relay died before commit): the events are
// published again. That is the at-least-once contract, and why consumers
// deduplicate on the event id. Order is kept within each delivery.
func TestPublishedButUncommittedIsPublishedAgain(t *testing.T) {
	pool := db.NewDB(t, 4)
	auctions := seed(t, pool, 2, 1)
	svc := auction.NewService(pool)
	bid(t, svc, auctions[0], 1, 100)
	bid(t, svc, auctions[0], 2, 101)

	topic := kafka.NewTopic(t, 1)
	pub := newPublisher(t, topic)
	if _, _, err := outbox.NewRelay(pool, failingPublisher{inner: pub}, 10, 5*time.Second, nil).PublishBatch(t.Context()); err == nil {
		t.Fatal("PublishBatch returned nil")
	}
	if n, _, err := outbox.NewRelay(pool, pub, 10, 5*time.Second, nil).PublishBatch(t.Context()); err != nil || n != 2 {
		t.Fatalf("second relay = %d, err %v; want 2, nil", n, err)
	}

	events := decode(t, kafka.ReadAll(t, topic, 4, 10*time.Second))
	var ids []int64
	for _, e := range events {
		ids = append(ids, e.ID)
	}
	if len(ids) != 4 || ids[0] != ids[2] || ids[1] != ids[3] || ids[0] >= ids[1] {
		t.Fatalf("event ids = %v, want two in-order deliveries of the same two events", ids)
	}
}

// R2: several relays running at once, while bids arrive on several
// auctions at once. Every event must be published exactly once (nothing
// fails here, so there is no reason for a duplicate), and each auction's
// events must arrive in outbox order.
func TestConcurrentRelaysKeepPerAuctionOrder(t *testing.T) {
	const (
		relays       = 4
		auctionCount = 5
		bidsEach     = 40
	)
	pool := db.NewDB(t, 20)
	auctions := seed(t, pool, 2, auctionCount)
	svc := auction.NewService(pool)
	topic := kafka.NewTopic(t, 3)

	ctx, cancel := context.WithCancel(t.Context())
	var running sync.WaitGroup
	for range relays {
		relay := outbox.NewRelay(pool, newPublisher(t, topic), 7, 5*time.Second, nil)
		running.Go(func() { relay.Run(ctx, 5*time.Millisecond, discard) })
	}

	// Two alternating users per auction, so every bid is accepted.
	var bidding sync.WaitGroup
	for _, a := range auctions {
		bidding.Go(func() {
			for i := range bidsEach {
				bid(t, svc, a, int64(1+i%2), int64(100+i))
			}
		})
	}
	bidding.Wait()

	total := auctionCount * bidsEach
	records := kafka.ReadAll(t, topic, total, 30*time.Second)
	// Stop the relays only once every row is marked. Having all records in
	// Kafka is not enough: the relay that sent the last batch may not have
	// committed its marks yet, and cancelling it then rolls them back (the
	// at-least-once window; those rows would be published again).
	deadline := time.Now().Add(10 * time.Second)
	for unpublished(t, pool) > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	running.Wait()

	events := decode(t, records)
	if len(events) != total {
		t.Fatalf("read %d events, want %d", len(events), total)
	}
	perAuction := map[int64][]int64{}
	seen := map[int64]bool{}
	for _, e := range events {
		if seen[e.ID] {
			t.Errorf("event %d published twice", e.ID)
		}
		seen[e.ID] = true
		perAuction[e.AuctionID] = append(perAuction[e.AuctionID], e.ID)
	}
	for a, ids := range perAuction {
		if !slices.IsSorted(ids) {
			t.Errorf("auction %d events out of order: %v", a, ids)
		}
		if len(ids) != bidsEach {
			t.Errorf("auction %d: %d events, want %d", a, len(ids), bidsEach)
		}
	}
	if got := unpublished(t, pool); got != 0 {
		t.Errorf("%d events left unpublished", got)
	}
}
