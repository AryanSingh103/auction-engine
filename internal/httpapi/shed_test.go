package httpapi

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestShedBidsRefusesBeyondTheCap(t *testing.T) {
	const max = 3
	release := make(chan struct{})
	var inside sync.WaitGroup
	inside.Add(max)
	var shed, admitted, entered atomic.Int64
	h := shedBids(max, slog.New(slog.NewTextHandler(io.Discard, nil)), func(s bool) {
		if s {
			shed.Add(1)
		} else {
			admitted.Add(1)
		}
	})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if entered.Add(1) <= max { // only the requests that fill the slots signal
			inside.Done()
		}
		<-release
		w.WriteHeader(http.StatusCreated)
	}))

	// Fill every slot with a request that waits inside the handler.
	codes := make(chan int, max)
	for range max {
		go func() {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/auctions/1/bids", nil))
			codes <- rec.Code
		}()
	}
	inside.Wait()

	// The next one is refused at once, without waiting for a slot.
	start := time.Now()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/auctions/1/bids", nil))
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "1" {
		t.Fatalf("over the cap: %d, Retry-After %q; want 503, 1", rec.Code, rec.Header().Get("Retry-After"))
	}
	if waited := time.Since(start); waited > 100*time.Millisecond {
		t.Errorf("shed response took %s; it must not wait for a slot", waited)
	}

	close(release)
	for range max {
		if c := <-codes; c != http.StatusCreated {
			t.Errorf("admitted request got %d, want 201", c)
		}
	}
	// Slots were freed: a new request is admitted.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/auctions/1/bids", nil))
	if rec.Code != http.StatusCreated {
		t.Errorf("after release: %d, want 201", rec.Code)
	}
	if shed.Load() != 1 || admitted.Load() != max+1 {
		t.Errorf("recorded %d shed, %d admitted; want 1, %d", shed.Load(), admitted.Load(), max+1)
	}
}
