// Package live delivers auction updates to WebSocket clients: a per-process
// hub of rooms (one per auction) fed by Redis pub/sub, so a bid accepted on
// any API instance reaches clients connected to every instance.
// See docs/decisions/020.
package live

import (
	"errors"
	"sync"
)

// ErrHubClosed is returned by Join once the hub has shut down.
var ErrHubClosed = errors.New("live hub is shut down")

// Drop reasons, reported to the connection so it can close with the right
// status.
const (
	ReasonSlow     = "slow"     // its send buffer filled up
	ReasonShutdown = "shutdown" // the server is going away
)

// Subscriber is one connection's membership in an auction's room.
type Subscriber struct {
	AuctionID int64
	// Send carries messages in publication order. It is buffered; when it
	// is full the hub drops the subscriber instead of waiting.
	Send chan []byte

	dropped chan struct{}
	once    sync.Once
	reason  string
}

// Dropped is closed when the hub removes the subscriber (see Reason).
func (s *Subscriber) Dropped() <-chan struct{} { return s.dropped }

// Reason says why the subscriber was dropped. Valid after Dropped closes.
func (s *Subscriber) Reason() string { return s.reason }

func (s *Subscriber) drop(reason string) {
	s.once.Do(func() {
		s.reason = reason
		close(s.dropped)
	})
}

// Hub fans messages out to the subscribers of each auction.
//
// Broadcast never blocks on a subscriber: each has a bounded buffer, and a
// subscriber whose buffer is full is dropped (its connection is closed and
// the client reconnects and resyncs). One slow client therefore costs
// itself a reconnect, never delays everyone else in the room, and cannot
// make the server buffer without limit.
type Hub struct {
	bufSize int
	onDrop  func(reason string)

	mu     sync.Mutex
	rooms  map[int64]map[*Subscriber]struct{}
	closed bool
}

// NewHub returns a hub whose subscribers buffer up to bufSize messages.
// onDrop, if non-nil, is called for every dropped subscriber.
func NewHub(bufSize int, onDrop func(reason string)) *Hub {
	if onDrop == nil {
		onDrop = func(string) {}
	}
	return &Hub{bufSize: bufSize, onDrop: onDrop, rooms: map[int64]map[*Subscriber]struct{}{}}
}

// Join adds a subscriber to an auction's room.
func (h *Hub) Join(auctionID int64) (*Subscriber, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, ErrHubClosed
	}
	s := &Subscriber{AuctionID: auctionID, Send: make(chan []byte, h.bufSize), dropped: make(chan struct{})}
	room := h.rooms[auctionID]
	if room == nil {
		room = map[*Subscriber]struct{}{}
		h.rooms[auctionID] = room
	}
	room[s] = struct{}{}
	return s, nil
}

// Leave removes a subscriber (its connection ended). Idempotent.
func (h *Hub) Leave(s *Subscriber) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.remove(s)
}

func (h *Hub) remove(s *Subscriber) {
	room := h.rooms[s.AuctionID]
	delete(room, s)
	if len(room) == 0 {
		delete(h.rooms, s.AuctionID)
	}
}

// Broadcast queues msg for every subscriber of the auction and returns how
// many received it and how many were dropped for being too slow.
func (h *Hub) Broadcast(auctionID int64, msg []byte) (delivered, dropped int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.rooms[auctionID] {
		select {
		case s.Send <- msg:
			delivered++
		default:
			h.remove(s)
			s.drop(ReasonSlow)
			h.onDrop(ReasonSlow)
			dropped++
		}
	}
	return delivered, dropped
}

// Rooms lists the auctions that currently have subscribers.
func (h *Hub) Rooms() []int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	ids := make([]int64, 0, len(h.rooms))
	for id := range h.rooms {
		ids = append(ids, id)
	}
	return ids
}

// Count is the number of subscribers across all rooms.
func (h *Hub) Count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, room := range h.rooms {
		n += len(room)
	}
	return n
}

// Close drops every subscriber with ReasonShutdown and refuses new ones.
// http.Server.Shutdown does not track hijacked (WebSocket) connections, so
// the server calls this from RegisterOnShutdown to close them cleanly.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for _, room := range h.rooms {
		for s := range room {
			s.drop(ReasonShutdown)
		}
	}
	h.rooms = map[int64]map[*Subscriber]struct{}{}
}
