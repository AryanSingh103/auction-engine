package live

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

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

// NewBus returns a Bus. record, if non-nil, receives ("publish", "ok"|"error")
// and ("receive", "ok"|"bad_message").
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
// to the hub's room for that auction. It returns once the subscription is
// confirmed; delivery continues in the background until ctx is cancelled,
// after which done is closed.
//
// While the Redis connection is down, go-redis reconnects and resubscribes
// on its own, but messages published meanwhile are lost. Clients detect
// that through the bid chain and the periodic sync (docs/decisions/020).
func (b *Bus) Forward(ctx context.Context, hub *Hub) (done <-chan struct{}, err error) {
	ps := b.rdb.PSubscribe(ctx, channelPrefix+"*"+channelSuffix)
	if _, err := ps.Receive(ctx); err != nil { // the subscription confirmation
		_ = ps.Close()
		return nil, fmt.Errorf("subscribe to auction events: %w", err)
	}
	d := make(chan struct{})
	go func() {
		defer close(d)
		defer func() { _ = ps.Close() }()
		msgs := ps.Channel()
		for {
			select {
			case <-ctx.Done():
				return
			case m, ok := <-msgs:
				if !ok {
					return
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
	}()
	return d, nil
}
