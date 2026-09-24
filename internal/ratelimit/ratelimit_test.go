package ratelimit

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AryanSingh103/auction-engine/internal/testredis"
)

var server *testredis.Server

func TestMain(m *testing.M) {
	ctx := context.Background()
	var err error
	server, err = testredis.Start(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ratelimit tests: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	_ = server.Terminate(ctx)
	os.Exit(code)
}

func TestBurstThenLimited(t *testing.T) {
	l := New(server.NewClient(t), "rl:", 10, 5) // 5 at once, 10/s refill
	ctx := t.Context()
	for i := range 5 {
		d, err := l.Allow(ctx, "u1")
		if err != nil || !d.Allowed {
			t.Fatalf("request %d within burst: %+v, %v", i+1, d, err)
		}
	}
	d, err := l.Allow(ctx, "u1")
	if err != nil || d.Allowed {
		t.Fatalf("6th request: %+v, %v; want limited", d, err)
	}
	// One token refills in 100ms at 10/s; allow for the time since the
	// bucket emptied.
	if d.RetryAfter <= 0 || d.RetryAfter > 100*time.Millisecond {
		t.Errorf("RetryAfter = %s, want (0, 100ms]", d.RetryAfter)
	}

	// Keys are independent.
	if d, err := l.Allow(ctx, "u2"); err != nil || !d.Allowed {
		t.Errorf("other key limited: %+v, %v", d, err)
	}

	// After waiting RetryAfter a token is available again.
	time.Sleep(d.RetryAfter + 20*time.Millisecond)
	if d, err := l.Allow(ctx, "u1"); err != nil || !d.Allowed {
		t.Errorf("after refill: %+v, %v; want allowed", d, err)
	}
}

// 200 concurrent requests against a burst of 10 with a negligible refill
// rate: exactly 10 may pass. A non-atomic read-modify-write would let
// several requests see the same last token.
func TestConcurrentRequestsCannotOverdraw(t *testing.T) {
	l := New(server.NewClient(t), "rl:", 0.001, 10)
	var allowed atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			d, err := l.Allow(t.Context(), "hot")
			if err != nil {
				t.Errorf("Allow: %v", err)
				return
			}
			if d.Allowed {
				allowed.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if got := allowed.Load(); got != 10 {
		t.Errorf("%d of 200 concurrent requests allowed, want exactly 10 (the burst)", got)
	}
}

func TestIdleBucketsExpire(t *testing.T) {
	c := server.NewClient(t)
	l := New(c, "rl:", 10, 5)
	if _, err := l.Allow(t.Context(), "u1"); err != nil {
		t.Fatal(err)
	}
	ttl, err := c.PTTL(t.Context(), "rl:u1").Result()
	if err != nil {
		t.Fatal(err)
	}
	// Full refill takes 5/10 s; the key must expire shortly after that.
	if ttl <= 0 || ttl > 1500*time.Millisecond {
		t.Errorf("bucket TTL = %s, want (0, 1.5s]", ttl)
	}
}

func TestRedisDownReturnsError(t *testing.T) {
	c := server.NewClient(t)
	l := New(c, "rl:", 10, 5)
	_ = c.Close()
	if _, err := l.Allow(context.Background(), "u1"); err == nil {
		t.Error("Allow on a closed client returned no error; callers could not tell to fail open")
	}
}
