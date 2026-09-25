package live

import (
	"errors"
	"fmt"
	"testing"
)

func TestBroadcastReachesOnlyItsRoom(t *testing.T) {
	h := NewHub(4, nil)
	a1, _ := h.Join(1)
	a2, _ := h.Join(1)
	b, _ := h.Join(2)

	if d, x := h.Broadcast(1, []byte("m")); d != 2 || x != 0 {
		t.Fatalf("Broadcast = %d delivered, %d dropped; want 2, 0", d, x)
	}
	for _, s := range []*Subscriber{a1, a2} {
		if got := string(<-s.Send); got != "m" {
			t.Errorf("room member got %q", got)
		}
	}
	select {
	case m := <-b.Send:
		t.Errorf("subscriber of auction 2 received %q", m)
	default:
	}
}

// One subscriber that stops reading must be dropped, while the others keep
// receiving every message, and Broadcast must never block.
func TestSlowSubscriberIsDroppedOthersUnaffected(t *testing.T) {
	var drops []string
	h := NewHub(2, func(r string) { drops = append(drops, r) })
	slow, _ := h.Join(1)
	fast, _ := h.Join(1)

	for i := range 5 {
		h.Broadcast(1, []byte(fmt.Sprint(i)))
		<-fast.Send // the fast subscriber keeps up
	}

	select {
	case <-slow.Dropped():
	default:
		t.Fatal("slow subscriber was not dropped after its buffer filled")
	}
	if slow.Reason() != ReasonSlow {
		t.Errorf("drop reason = %q, want %q", slow.Reason(), ReasonSlow)
	}
	if fmt.Sprint(drops) != "[slow]" {
		t.Errorf("onDrop calls = %v, want exactly one slow drop", drops)
	}
	if h.Count() != 1 {
		t.Errorf("hub has %d subscribers, want 1 (the fast one)", h.Count())
	}
	// The slow one kept the messages that fit before it was dropped.
	if len(slow.Send) != 2 {
		t.Errorf("slow subscriber holds %d messages, want 2 (its buffer)", len(slow.Send))
	}
}

func TestLeaveRemovesEmptyRooms(t *testing.T) {
	h := NewHub(1, nil)
	s, _ := h.Join(9)
	h.Leave(s)
	h.Leave(s) // idempotent
	if len(h.Rooms()) != 0 {
		t.Errorf("rooms after last Leave = %v, want none", h.Rooms())
	}
}

func TestCloseDropsEveryoneAndRefusesNew(t *testing.T) {
	h := NewHub(1, nil)
	s1, _ := h.Join(1)
	s2, _ := h.Join(2)
	h.Close()
	for _, s := range []*Subscriber{s1, s2} {
		select {
		case <-s.Dropped():
			if s.Reason() != ReasonShutdown {
				t.Errorf("reason = %q, want shutdown", s.Reason())
			}
		default:
			t.Error("subscriber not dropped on Close")
		}
	}
	if _, err := h.Join(1); !errors.Is(err, ErrHubClosed) {
		t.Errorf("Join after Close err = %v, want ErrHubClosed", err)
	}
}
