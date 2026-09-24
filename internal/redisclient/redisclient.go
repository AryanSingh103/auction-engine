// Package redisclient builds the API's Redis client.
//
// Redis is an accelerator here, never the source of truth: it holds a
// read cache, rate-limit buckets, and a pub/sub fan-out of events that are
// also derivable from Postgres. So every Redis call has a short timeout, and
// callers degrade (serve from Postgres, allow the request, skip a live
// update) instead of failing when Redis is slow or down.
// See docs/decisions/017.
package redisclient

import (
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// New returns a client for url (redis://host:port/db) whose dial, read and
// write operations each give up after timeout.
func New(url string, timeout time.Duration) (*redis.Client, error) {
	opts, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("parse REDIS_URL: %w", err)
	}
	opts.DialTimeout = timeout
	opts.ReadTimeout = timeout
	opts.WriteTimeout = timeout
	// Retries would multiply the timeout on every call while Redis is down;
	// callers already degrade, so one attempt is enough.
	opts.MaxRetries = 0
	return redis.NewClient(opts), nil
}
