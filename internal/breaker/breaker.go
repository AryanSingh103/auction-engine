// Package breaker is a circuit breaker for calls to an unreliable
// dependency (the payment provider, docs/decisions/024).
//
// Closed: calls go through, and consecutive failures are counted. After
// Threshold of them it opens: calls are refused at once, without touching
// the dependency, for OpenFor. Then it is half-open: exactly one trial call
// is let through. Success closes it; failure opens it again.
//
// The point is to stop hammering a dependency that is down (every call
// would time out anyway, holding resources and delaying recovery) and to
// fail fast, so the caller can back off as a whole instead of per call.
package breaker

import (
	"errors"
	"sync"
	"time"
)

// ErrOpen is returned by Allow while the breaker refuses calls.
var ErrOpen = errors.New("circuit breaker open")

// State is the breaker's state.
type State int

const (
	Closed State = iota
	Open
	HalfOpen
)

func (s State) String() string {
	switch s {
	case Closed:
		return "closed"
	case Open:
		return "open"
	default:
		return "half-open"
	}
}

// Breaker is safe for concurrent use.
type Breaker struct {
	threshold int
	openFor   time.Duration
	now       func() time.Time

	mu       sync.Mutex
	state    State
	failures int       // consecutive, while closed
	openedAt time.Time // when it last opened
	trialOut bool      // a half-open trial call is in flight
	onChange func(from, to State)
}

// New returns a closed breaker. now is the clock (time.Now in production);
// onChange, if non-nil, is called on every transition, under the lock.
func New(threshold int, openFor time.Duration, now func() time.Time, onChange func(from, to State)) *Breaker {
	return &Breaker{threshold: threshold, openFor: openFor, now: now, onChange: onChange}
}

// Allow asks to make one call. On nil the caller must make the call and
// then report it with Record; on ErrOpen it must not call.
func (b *Breaker) Allow() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case Open:
		if b.now().Sub(b.openedAt) < b.openFor {
			return ErrOpen
		}
		b.set(HalfOpen)
		b.trialOut = true
		return nil
	case HalfOpen:
		if b.trialOut {
			return ErrOpen // one trial at a time
		}
		b.trialOut = true
		return nil
	}
	return nil
}

// Record reports the outcome of an allowed call. success means the
// dependency answered properly (including a valid "no", such as a
// declined card); failure means it did not (error, 5xx, timeout).
func (b *Breaker) Record(success bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case HalfOpen:
		b.trialOut = false
		if success {
			b.failures = 0
			b.set(Closed)
		} else {
			b.open()
		}
	case Closed:
		if success {
			b.failures = 0
			return
		}
		b.failures++
		if b.failures >= b.threshold {
			b.open()
		}
	case Open:
		// A call allowed before the breaker opened finished late; its
		// outcome says nothing new.
	}
}

// State returns the current state (Open even if OpenFor has passed, until
// the next Allow moves it to half-open).
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

// RetryAfter is how long until an open breaker lets a trial through (0 if
// it is not open).
func (b *Breaker) RetryAfter() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state != Open {
		return 0
	}
	return max(0, b.openFor-b.now().Sub(b.openedAt))
}

func (b *Breaker) open() {
	b.failures = 0
	b.openedAt = b.now()
	b.set(Open)
}

func (b *Breaker) set(s State) {
	if s == b.state {
		return
	}
	from := b.state
	b.state = s
	if b.onChange != nil {
		b.onChange(from, s)
	}
}
