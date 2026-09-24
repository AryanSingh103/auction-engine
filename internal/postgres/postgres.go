// Package postgres builds the API's connection pool with the session
// settings every connection must carry.
package postgres

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PoolConfig are the pool settings that come from configuration.
type PoolConfig struct {
	URL             string
	MaxConns        int32
	IdleInTxTimeout time.Duration
}

// NewPool returns a pool whose every connection has
// idle_in_transaction_session_timeout set. Without it, a transaction whose
// client vanished (host crash, network partition) between statements keeps
// its row locks until TCP keepalive gives up, which defaults to about two
// hours; every bid on that auction, and in milestone 5 the closer, would
// block behind it.
//
// The setting is sent as a startup parameter, so it applies from the first
// statement on each connection, with no extra round trip.
func NewPool(ctx context.Context, c PoolConfig) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(c.URL)
	if err != nil {
		return nil, fmt.Errorf("parse database URL: %w", err)
	}
	cfg.MaxConns = c.MaxConns
	cfg.ConnConfig.RuntimeParams["idle_in_transaction_session_timeout"] = strconv.FormatInt(c.IdleInTxTimeout.Milliseconds(), 10)

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create database pool: %w", err)
	}
	return pool, nil
}
