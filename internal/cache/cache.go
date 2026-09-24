// Package cache holds a Redis read cache of auction state for
// GET /auctions/{id}. It is only ever used for display: the bid path reads
// Postgres under a row lock and never consults it. See docs/decisions/019.
package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/AryanSingh103/auction-engine/internal/auction"
)

// putIfNewer stores the value only if no newer version is cached.
//
// Without the version check, cache-aside has a classic race: a reader
// misses and loads state S1 from Postgres, then a bid commits and refreshes
// the cache to S2, and only then does the slow reader write S1 over S2,
// leaving stale state until the TTL expires. With it, writes can arrive in
// any order and the cache only ever moves forward.
//
// KEYS[1] key. ARGV[1] version, ARGV[2] JSON, ARGV[3] TTL in ms.
// Returns 1 if stored, 0 if a newer or equal version was already cached.
var putIfNewer = redis.NewScript(`
local cur = redis.call('HGET', KEYS[1], 'v')
if cur and tonumber(cur) >= tonumber(ARGV[1]) then
  return 0
end
redis.call('HSET', KEYS[1], 'v', ARGV[1], 'data', ARGV[2])
redis.call('PEXPIRE', KEYS[1], ARGV[3])
return 1
`)

// Version orders states of one auction. The head bid id only grows (the
// chain is append-only) and an auction only ever goes open -> closed, after
// which its head can no longer change. So 2*head + closed strictly
// increases with every state change.
func Version(a auction.Auction) int64 {
	var v int64
	if a.Head != nil {
		v = 2 * a.Head.BidID
	}
	if a.Status == auction.StatusClosed {
		v++
	}
	return v
}

// Auctions caches auction state in Redis.
type Auctions struct {
	rdb    *redis.Client
	ttl    time.Duration
	record func(op, result string)
}

// New returns a cache whose entries live for ttl. record, if non-nil,
// receives ("get", "hit"|"miss"|"error") and ("put", "stored"|"stale"|"error").
func New(rdb *redis.Client, ttl time.Duration, record func(op, result string)) *Auctions {
	if record == nil {
		record = func(string, string) {}
	}
	return &Auctions{rdb: rdb, ttl: ttl, record: record}
}

func key(id int64) string { return "auction:" + strconv.FormatInt(id, 10) }

// Get returns the cached state of auction id, if present.
func (c *Auctions) Get(ctx context.Context, id int64) (auction.Auction, bool, error) {
	data, err := c.rdb.HGet(ctx, key(id), "data").Bytes()
	if errors.Is(err, redis.Nil) {
		c.record("get", "miss")
		return auction.Auction{}, false, nil
	}
	if err != nil {
		c.record("get", "error")
		return auction.Auction{}, false, fmt.Errorf("cache get auction %d: %w", id, err)
	}
	var a auction.Auction
	if err := json.Unmarshal(data, &a); err != nil {
		c.record("get", "error")
		return auction.Auction{}, false, fmt.Errorf("cache decode auction %d: %w", id, err)
	}
	c.record("get", "hit")
	return a, true, nil
}

// Put caches a unless a newer version is already cached.
func (c *Auctions) Put(ctx context.Context, a auction.Auction) error {
	data, err := json.Marshal(a)
	if err != nil {
		c.record("put", "error")
		return fmt.Errorf("cache encode auction %d: %w", a.ID, err)
	}
	stored, err := putIfNewer.Run(ctx, c.rdb, []string{key(a.ID)}, Version(a), data, c.ttl.Milliseconds()).Int()
	if err != nil {
		c.record("put", "error")
		return fmt.Errorf("cache put auction %d: %w", a.ID, err)
	}
	if stored == 1 {
		c.record("put", "stored")
	} else {
		c.record("put", "stale")
	}
	return nil
}
