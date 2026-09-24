package cache

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/AryanSingh103/auction-engine/internal/auction"
	"github.com/AryanSingh103/auction-engine/internal/testredis"
)

var server *testredis.Server

func TestMain(m *testing.M) {
	ctx := context.Background()
	var err error
	server, err = testredis.Start(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cache tests: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	_ = server.Terminate(ctx)
	os.Exit(code)
}

func state(head int64, status auction.Status) auction.Auction {
	a := auction.Auction{ID: 7, ItemID: 1, StartingPrice: 1000, MinIncrement: 100, Status: status,
		StartAt: time.Unix(1_800_000_000, 0).UTC(), EndAt: time.Unix(1_800_003_600, 0).UTC()}
	if head > 0 {
		a.Head = &auction.Head{BidID: head, UserID: head, Price: 1000 + head*100}
	}
	return a
}

func TestVersionOrdersEveryStateChange(t *testing.T) {
	states := []auction.Auction{
		state(0, auction.StatusOpen), state(0, auction.StatusClosed),
		state(5, auction.StatusOpen), state(5, auction.StatusClosed), state(9, auction.StatusOpen),
	}
	for i := 1; i < len(states); i++ {
		if Version(states[i]) <= Version(states[i-1]) {
			t.Errorf("version of state %d (%d) not greater than state %d (%d)", i, Version(states[i]), i-1, Version(states[i-1]))
		}
	}
}

func TestPutGetRoundTrip(t *testing.T) {
	var got []string
	c := New(server.NewClient(t), time.Minute, func(op, r string) { got = append(got, op+":"+r) })
	ctx := t.Context()

	if _, ok, err := c.Get(ctx, 7); err != nil || ok {
		t.Fatalf("empty cache Get = ok %v, err %v; want miss", ok, err)
	}
	want := state(3, auction.StatusOpen)
	if err := c.Put(ctx, want); err != nil {
		t.Fatal(err)
	}
	a, ok, err := c.Get(ctx, 7)
	if err != nil || !ok {
		t.Fatalf("Get after Put = ok %v, err %v", ok, err)
	}
	if a.Head == nil || *a.Head != *want.Head || !a.EndAt.Equal(want.EndAt) || a.Status != want.Status {
		t.Errorf("round trip = %+v (head %+v), want %+v (head %+v)", a, a.Head, want, want.Head)
	}
	if fmt.Sprint(got) != "[get:miss put:stored get:hit]" {
		t.Errorf("recorded %v", got)
	}
}

// The race the version guard exists for: a slow reader tries to cache an
// older state after a newer one was cached. The newer state must survive.
func TestOlderStateNeverOverwritesNewer(t *testing.T) {
	c := New(server.NewClient(t), time.Minute, nil)
	ctx := t.Context()

	if err := c.Put(ctx, state(9, auction.StatusOpen)); err != nil {
		t.Fatal(err)
	}
	if err := c.Put(ctx, state(4, auction.StatusOpen)); err != nil { // the slow reader
		t.Fatal(err)
	}
	a, _, err := c.Get(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	if a.Head == nil || a.Head.BidID != 9 {
		t.Fatalf("cached head = %+v, want bid 9 (the newer state)", a.Head)
	}

	// Closing the auction without a new bid is still a newer state.
	if err := c.Put(ctx, state(9, auction.StatusClosed)); err != nil {
		t.Fatal(err)
	}
	if a, _, _ := c.Get(ctx, 7); a.Status != auction.StatusClosed {
		t.Errorf("status = %s, want closed", a.Status)
	}
}

func TestEntriesExpire(t *testing.T) {
	rdb := server.NewClient(t)
	c := New(rdb, 1500*time.Millisecond, nil)
	if err := c.Put(t.Context(), state(1, auction.StatusOpen)); err != nil {
		t.Fatal(err)
	}
	ttl, err := rdb.PTTL(t.Context(), "auction:7").Result()
	if err != nil || ttl <= 0 || ttl > 1500*time.Millisecond {
		t.Errorf("PTTL = %s, %v; want (0, 1.5s]", ttl, err)
	}
}
