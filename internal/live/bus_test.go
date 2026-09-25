package live

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AryanSingh103/auction-engine/internal/auction"
	"github.com/AryanSingh103/auction-engine/internal/redisclient"
	"github.com/AryanSingh103/auction-engine/internal/testredis"
)

var redisServer *testredis.Server

func TestMain(m *testing.M) {
	ctx := context.Background()
	var err error
	redisServer, err = testredis.Start(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "live tests: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	_ = redisServer.Terminate(ctx)
	os.Exit(code)
}

func TestChannelNames(t *testing.T) {
	if id, ok := auctionIDFromChannel(channel(42)); !ok || id != 42 {
		t.Errorf("round trip = %d, %v", id, ok)
	}
	for _, bad := range []string{"auction:x:events", "other:1:events", "auction:1:other"} {
		if _, ok := auctionIDFromChannel(bad); ok {
			t.Errorf("%q parsed as an auction channel", bad)
		}
	}
}

// Two "instances" (two Bus+Hub pairs on one Redis): a bid published by one
// reaches subscribers of the other, and only in the right room.
func TestBidCrossesInstances(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	rdb := redisServer.NewClient(t)

	hubB := NewHub(8, nil)
	busA, busB := NewBus(rdb, nil), NewBus(rdb, nil)
	readyB, doneB := busB.Forward(ctx, hubB)
	<-readyB
	subB, _ := hubB.Join(5)
	otherRoom, _ := hubB.Join(6)

	prev := int64(10)
	if err := busA.PublishBid(ctx, auction.Bid{ID: 11, AuctionID: 5, UserID: 2, Amount: 1500, PrevBidID: &prev}); err != nil {
		t.Fatal(err)
	}

	select {
	case raw := <-subB.Send:
		var m BidMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("message is not JSON: %v", err)
		}
		if m.Type != TypeBid || m.Bid.ID != 11 || m.Bid.PrevBidID == nil || *m.Bid.PrevBidID != 10 {
			t.Errorf("received %+v", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("bid published on instance A never reached instance B")
	}
	select {
	case m := <-otherRoom.Send:
		t.Errorf("subscriber of auction 6 received %s", m)
	case <-time.After(100 * time.Millisecond):
	}

	cancel()
	select {
	case <-doneB:
	case <-time.After(2 * time.Second):
		t.Error("Forward did not stop after its context was cancelled")
	}
}

// Started while Redis is unreachable, Forward must keep retrying and begin
// delivering once Redis is reachable, rather than disabling live updates
// for the life of the process.
func TestForwardRetriesUntilRedisIsReachable(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	down, err := redisclient.New("redis://127.0.0.1:1/0", 100*time.Millisecond) // nothing listens here
	if err != nil {
		t.Fatal(err)
	}
	var subscribeErrors atomic.Int64
	bus := NewBus(down, func(op, result string) {
		if op == "subscribe" && result == "error" {
			subscribeErrors.Add(1)
		}
	})
	ready, done := bus.Forward(ctx, NewHub(1, nil))

	deadline := time.Now().Add(3 * time.Second)
	for subscribeErrors.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := subscribeErrors.Load(); n < 3 {
		t.Fatalf("%d subscribe attempts, want it to keep retrying", n)
	}
	select {
	case <-ready:
		t.Fatal("ready closed although Redis was never reachable")
	default:
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Forward did not stop after cancellation while retrying")
	}
}
