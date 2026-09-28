package breaker

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTest(threshold int) (*Breaker, *clock, *[]string) {
	c := &clock{t: time.Unix(0, 0)}
	var changes []string
	b := New(threshold, 10*time.Second, c.now, func(from, to State) { changes = append(changes, from.String()+">"+to.String()) })
	return b, c, &changes
}

// call makes one call that the breaker must allow, and records it.
func call(t *testing.T, b *Breaker, success bool) {
	t.Helper()
	if err := b.Allow(); err != nil {
		t.Fatalf("Allow = %v, want the call allowed", err)
	}
	b.Record(success)
}

func TestOpensAfterConsecutiveFailures(t *testing.T) {
	b, _, _ := newTest(3)
	call(t, b, false)
	call(t, b, false)
	call(t, b, true) // resets the count
	call(t, b, false)
	call(t, b, false)
	if b.State() != Closed {
		t.Fatalf("state after 2 consecutive failures = %s, want closed", b.State())
	}
	call(t, b, false)
	if b.State() != Open {
		t.Fatalf("state after 3 consecutive failures = %s, want open", b.State())
	}
	if err := b.Allow(); !errors.Is(err, ErrOpen) {
		t.Fatalf("Allow while open = %v, want ErrOpen", err)
	}
}

func TestHalfOpenTrial(t *testing.T) {
	for _, tt := range []struct {
		name    string
		success bool
		want    State
	}{
		{"trial succeeds", true, Closed},
		{"trial fails", false, Open},
	} {
		t.Run(tt.name, func(t *testing.T) {
			b, c, changes := newTest(1)
			call(t, b, false) // opens
			c.advance(9 * time.Second)
			if err := b.Allow(); !errors.Is(err, ErrOpen) {
				t.Fatalf("Allow before OpenFor = %v, want ErrOpen", err)
			}
			if got := b.RetryAfter(); got != time.Second {
				t.Errorf("RetryAfter = %s, want 1s", got)
			}
			c.advance(time.Second)
			if err := b.Allow(); err != nil {
				t.Fatalf("trial Allow = %v, want nil", err)
			}
			if err := b.Allow(); !errors.Is(err, ErrOpen) {
				t.Fatalf("second Allow during the trial = %v, want ErrOpen", err)
			}
			b.Record(tt.success)
			if b.State() != tt.want {
				t.Fatalf("state after trial = %s, want %s (changes %v)", b.State(), tt.want, *changes)
			}
			if tt.want == Open {
				// Reopened now, so a full OpenFor must pass again.
				c.advance(9 * time.Second)
				if err := b.Allow(); !errors.Is(err, ErrOpen) {
					t.Fatalf("Allow 9s after reopening = %v, want ErrOpen", err)
				}
			}
		})
	}
}

func TestLateResultWhileOpenIsIgnored(t *testing.T) {
	b, _, _ := newTest(1)
	if err := b.Allow(); err != nil { // allowed while closed...
		t.Fatal(err)
	}
	call(t, b, false) // ...another call opens the breaker meanwhile
	b.Record(true)    // the first call finishes late
	if b.State() != Open {
		t.Fatalf("state = %s, want still open", b.State())
	}
}

// Under concurrency, a half-open breaker lets exactly one trial through.
func TestOneTrialUnderConcurrency(t *testing.T) {
	b, c, _ := newTest(1)
	call(t, b, false)
	c.advance(10 * time.Second)
	var allowed atomic.Int64
	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			if b.Allow() == nil {
				allowed.Add(1)
			}
		})
	}
	wg.Wait()
	if n := allowed.Load(); n != 1 {
		t.Fatalf("%d calls allowed while half-open, want 1", n)
	}
}
