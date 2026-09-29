package closer_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AryanSingh103/auction-engine/internal/auction"
	"github.com/AryanSingh103/auction-engine/internal/closer"
	"github.com/AryanSingh103/auction-engine/internal/invariants"
	"github.com/AryanSingh103/auction-engine/internal/testdb"
)

var server *testdb.Server

func TestMain(m *testing.M) {
	ctx := context.Background()
	var err error
	if server, err = testdb.Start(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "closer tests: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	_ = server.Terminate(ctx)
	os.Exit(code)
}

const interval = 50 * time.Millisecond

// leaders counts closers that currently report leading, and the most seen
// at once.
type leaders struct {
	mu       sync.Mutex
	now, max int
}

func (l *leaders) Leader(leading bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if leading {
		l.now++
		l.max = max(l.max, l.now)
	} else {
		l.now--
	}
}

func (l *leaders) Closed(string, time.Duration) {}

func (l *leaders) current() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.now
}

// seed creates an item and n auctions ending at endExpr, returning their ids.
func seed(t *testing.T, pool *pgxpool.Pool, n int, endExpr string) []int64 {
	t.Helper()
	ids := make([]int64, n)
	for i := range ids {
		if err := pool.QueryRow(t.Context(), fmt.Sprintf(`
			WITH item AS (INSERT INTO items (title) VALUES ('unit') RETURNING id)
			INSERT INTO auctions (item_id, start_at, end_at, starting_price, min_increment)
			SELECT id, clock_timestamp() - interval '1 day', %s, 1000, 100 FROM item RETURNING id`, endExpr)).Scan(&ids[i]); err != nil {
			t.Fatalf("seed auction: %v", err)
		}
	}
	return ids
}

// start runs n closers until the test ends and returns their cancel funcs.
func start(t *testing.T, pool *pgxpool.Pool, n int, obs closer.Observer) []context.CancelFunc {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := auction.NewService(pool)
	var wg sync.WaitGroup
	cancels := make([]context.CancelFunc, n)
	for i := range cancels {
		ctx, cancel := context.WithCancel(context.Background())
		cancels[i] = cancel
		wg.Go(func() { closer.New(pool, svc, interval, 2, obs, log).Run(ctx) })
	}
	t.Cleanup(func() {
		for _, c := range cancels {
			c()
		}
		wg.Wait()
	})
	return cancels
}

// waitFor polls cond until it holds or 10s pass.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func openCount(t *testing.T, pool *pgxpool.Pool, ids []int64) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM auctions WHERE id = ANY($1) AND status = 'open'`, ids).Scan(&n); err != nil {
		t.Fatalf("count open: %v", err)
	}
	return n
}

// leaderPID is the backend holding the closer's lock, or 0.
func leaderPID(t *testing.T, pool *pgxpool.Pool) int32 {
	t.Helper()
	var pid int32
	err := pool.QueryRow(t.Context(), `
		SELECT coalesce(max(pid), 0) FROM pg_locks
		WHERE locktype = 'advisory' AND granted AND ((classid::bigint << 32) | objid::bigint) = $1`,
		closer.LockKey).Scan(&pid)
	if err != nil {
		t.Fatalf("leader pid: %v", err)
	}
	return pid
}

func assertInvariants(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	v, _, err := invariants.Run(t.Context(), pool, invariants.SafetyMode)
	if err != nil || len(v) > 0 {
		t.Fatalf("invariants: %v, %v", v, err)
	}
}

// Three closers, one leader; every ended auction closes (batch 2, so the
// leader must rescan) and the one still running does not.
func TestOneLeaderClosesEndedAuctions(t *testing.T) {
	pool := server.NewDB(t, 12)
	ended := seed(t, pool, 5, "clock_timestamp() - interval '1 second'")
	running := seed(t, pool, 1, "clock_timestamp() + interval '1 hour'")

	obs := &leaders{}
	start(t, pool, 3, obs)
	waitFor(t, "ended auctions to close", func() bool { return openCount(t, pool, ended) == 0 })

	if n := openCount(t, pool, running); n != 1 {
		t.Errorf("running auction closed early")
	}
	obs.mu.Lock()
	most := obs.max
	obs.mu.Unlock()
	if most != 1 {
		t.Errorf("most leaders at once = %d, want 1", most)
	}
	assertInvariants(t, pool)
}

// The leader's session is killed (as a network partition or a Postgres
// restart would); the lock is released with it and another closer leads.
// So is a graceful stop of the new leader.
func TestLeadershipMovesWhenTheLeaderGoes(t *testing.T) {
	pool := server.NewDB(t, 12)
	obs := &leaders{}
	cancels := start(t, pool, 2, obs)

	var first int32
	waitFor(t, "a leader", func() bool { first = leaderPID(t, pool); return first != 0 })
	if _, err := pool.Exec(t.Context(), `SELECT pg_terminate_backend($1)`, first); err != nil {
		t.Fatalf("terminate leader: %v", err)
	}
	var second int32
	waitFor(t, "a new leader", func() bool { second = leaderPID(t, pool); return second != 0 && second != first })

	ids := seed(t, pool, 2, "clock_timestamp()")
	waitFor(t, "auctions to close under the new leader", func() bool { return openCount(t, pool, ids) == 0 })

	// Stop both; the lock must be released (no leader, no one reporting).
	for _, c := range cancels {
		c()
	}
	waitFor(t, "the lock to be released", func() bool { return leaderPID(t, pool) == 0 && obs.current() == 0 })
	assertInvariants(t, pool)
}
