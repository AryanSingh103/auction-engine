// Package ratelimit implements a token bucket shared by every API instance,
// stored in Redis. See docs/decisions/018.
package ratelimit

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// tokenBucket refills and takes one token atomically. Redis runs a script
// to completion before serving any other command, so two instances can
// never both read "1 token left" and both take it (the race a GET-then-SET
// in Go would have).
//
// It reads Redis's own clock (TIME) rather than taking "now" from the
// caller, so API instances with skewed clocks still agree on refill.
//
// KEYS[1] bucket key. ARGV[1] rate (tokens per second), ARGV[2] burst.
// Returns {allowed (0 or 1), milliseconds until a token is available}.
var tokenBucket = redis.NewScript(`
local rate  = tonumber(ARGV[1])
local burst = tonumber(ARGV[2])
local t = redis.call('TIME')
local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)

local b = redis.call('HMGET', KEYS[1], 'tokens', 'ts')
local tokens = tonumber(b[1]) or burst
local ts = tonumber(b[2]) or now
tokens = math.min(burst, tokens + math.max(0, now - ts) * rate / 1000)

local allowed, wait_ms = 0, 0
if tokens >= 1 then
  tokens = tokens - 1
  allowed = 1
else
  wait_ms = math.ceil((1 - tokens) * 1000 / rate)
end

redis.call('HSET', KEYS[1], 'tokens', tostring(tokens), 'ts', now)
-- An idle bucket refills completely within burst/rate seconds, after which
-- it is indistinguishable from a missing key: let Redis drop it.
redis.call('PEXPIRE', KEYS[1], math.ceil(burst * 1000 / rate) + 1000)
return {allowed, wait_ms}
`)

// Limiter is a token bucket per key: Burst requests at once, refilled at
// Rate per second.
type Limiter struct {
	rdb    *redis.Client
	rate   float64
	burst  int
	prefix string
}

// New returns a Limiter whose Redis keys start with prefix.
func New(rdb *redis.Client, prefix string, rate float64, burst int) *Limiter {
	return &Limiter{rdb: rdb, rate: rate, burst: burst, prefix: prefix}
}

// Decision is the result of one Allow call.
type Decision struct {
	Allowed bool
	// RetryAfter is how long until a token is available; 0 when allowed.
	RetryAfter time.Duration
}

// Allow takes one token from key's bucket if one is available.
func (l *Limiter) Allow(ctx context.Context, key string) (Decision, error) {
	res, err := tokenBucket.Run(ctx, l.rdb, []string{l.prefix + key}, l.rate, l.burst).Int64Slice()
	if err != nil {
		return Decision{}, fmt.Errorf("rate limit %q: %w", key, err)
	}
	if len(res) != 2 {
		return Decision{}, fmt.Errorf("rate limit %q: unexpected script result %v", key, res)
	}
	return Decision{Allowed: res[0] == 1, RetryAfter: time.Duration(res[1]) * time.Millisecond}, nil
}
