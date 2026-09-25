package live

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/AryanSingh103/auction-engine/internal/auction"
)

// Message types sent to WebSocket clients.
const (
	TypeSnapshot = "snapshot" // full auction state, first message on a connection
	TypeBid      = "bid"      // a newly accepted bid
	TypeSync     = "sync"     // periodic current head, to detect a lost final bid
)

// BidMessage announces an accepted bid. Clients apply it only if
// Bid.PrevBidID equals the head they already have; otherwise they missed a
// message (pub/sub is fire-and-forget) and must resync from a snapshot.
type BidMessage struct {
	Type string      `json:"type"`
	Bid  auction.Bid `json:"bid"`
}

const channelPrefix, channelSuffix = "auction:", ":events"

func channel(auctionID int64) string {
	return channelPrefix + strconv.FormatInt(auctionID, 10) + channelSuffix
}

func auctionIDFromChannel(ch string) (int64, bool) {
	s, ok := strings.CutPrefix(ch, channelPrefix)
	if !ok {
		return 0, false
	}
	s, ok = strings.CutSuffix(s, channelSuffix)
	if !ok {
		return 0, false
	}
	id, err := strconv.ParseInt(s, 10, 64)
	return id, err == nil
}

// Bus carries bid events between API instances over Redis pub/sub.
type Bus struct {
	rdb    *redis.Client
	record func(op, result string)
}

// NewBus returns a Bus. record, if non-nil, receives ("publish", "ok"|"error"),
// ("subscribe", "ok"|"error") and ("receive", "ok"|"bad_message").
func NewBus(rdb *redis.Client, record func(op, result string)) *Bus {
	if record == nil {
		record = func(string, string) {}
	}
	return &Bus{rdb: rdb, record: record}
}

// PublishBid implements auction.Publisher.
func (b *Bus) PublishBid(ctx context.Context, bid auction.Bid) error {
	msg, err := json.Marshal(BidMessage{Type: TypeBid, Bid: bid})
	if err != nil {
		b.record("publish", "error")
		return err
	}
	if err := b.rdb.Publish(ctx, channel(bid.AuctionID), msg).Err(); err != nil {
		b.record("publish", "error")
		return fmt.Errorf("publish bid %d: %w", bid.ID, err)
	}
	b.record("publish", "ok")
	return nil
}

// Forward subscribes to every auction's channel and broadcasts each message
// to the hub's room for that auction, until ctx is cancelled (then done is
// closed). ready is closed the first time the subscription is confirmed.
//
// If Redis is unreachable it keeps retrying with capped backoff instead of
// giving up: an instance that started while Redis was down must still get
// live updates once Redis is back (found by the M3 adversarial review).
// Once subscribed, go-redis reconnects and resubscribes on its own, but
// messages published meanwhile are lost; clients detect that through the
// bid chain and the periodic sync (docs/decisions/020).
func (b *Bus) Forward(ctx context.Context, hub *Hub) (ready, done <-chan struct{}) {
	readyCh, d := make(chan struct{}), make(chan struct{})
	go func() {
		r := readyCh
		defer close(d)
		backoff := 100 * time.Millisecond
		for ctx.Err() == nil {
			ps := b.rdb.PSubscribe(ctx, channelPrefix+"*"+channelSuffix)
			if _, err := ps.Receive(ctx); err != nil { // the subscription confirmation
				_ = ps.Close()
				b.record("subscribe", "error")
				select {
				case <-ctx.Done():
					return
				case <-time.After(backoff):
				}
				backoff = min(2*backoff, 5*time.Second)
				continue
			}
			b.record("subscribe", "ok")
			if r != nil {
				close(r)
				r = nil
			}
			b.deliver(ctx, ps, hub)
			_ = ps.Close()
		}
	}()
	return readyCh, d
}

func (b *Bus) deliver(ctx context.Context, ps *redis.PubSub, hub *Hub) {
	msgs := ps.Channel()
	for {
		select {
		case <-ctx.Done():
			return
		case m, ok := <-msgs:
			if !ok {
				return // the subscription ended; Forward resubscribes
			}
			id, ok := auctionIDFromChannel(m.Channel)
			if !ok {
				b.record("receive", "bad_message")
				continue
			}
			b.record("receive", "ok")
			hub.Broadcast(id, []byte(m.Payload))
		}
	}
}
