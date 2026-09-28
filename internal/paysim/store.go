// Package paysim is a deliberately unreliable fake payment provider
// (docs/decisions/023). It behaves like a real one in the ways that matter
// for invariant 5: a charge is keyed by an idempotency key, a repeat of the
// same request returns the original charge, and the charge is durable, so
// even a provider restart cannot turn a retry into a second charge.
package paysim

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrKeyReused is returned when an idempotency key comes back with a
// different request: a client bug, never retried.
var ErrKeyReused = errors.New("idempotency key reused with a different request")

// Charge is one successful charge.
type Charge struct {
	ID         string    `json:"id"`
	Key        string    `json:"-"`
	Amount     int64     `json:"amount"`
	CustomerID int64     `json:"customer_id"`
	CreatedAt  time.Time `json:"created_at"`
}

// Store keeps charges in their own schema. The fake provider shares the
// Postgres server for convenience only: nothing in the application reads
// or writes this schema except the invariant checker, which audits it the
// way a reconciliation job would audit a real provider's records.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore returns a store on pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Migrate creates the schema. The simulator owns it, so it is not part of
// the application's migrations.
func (s *Store) Migrate(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `
		CREATE SCHEMA IF NOT EXISTS paysim;
		CREATE TABLE IF NOT EXISTS paysim.charges (
			idempotency_key TEXT        PRIMARY KEY,
			id              TEXT        NOT NULL UNIQUE,
			amount          BIGINT      NOT NULL CHECK (amount > 0),
			customer_id     BIGINT      NOT NULL,
			created_at      TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
		);`)
	if err != nil {
		return fmt.Errorf("create paysim schema: %w", err)
	}
	return nil
}

// Charge charges amount to customer under key, or returns the charge
// already made under key (replayed = true). Concurrent calls with one key
// produce one charge: the primary key decides which insert wins.
func (s *Store) Charge(ctx context.Context, key string, amount, customer int64) (c Charge, replayed bool, err error) {
	id, err := newChargeID()
	if err != nil {
		return Charge{}, false, err
	}
	err = s.pool.QueryRow(ctx, `
		INSERT INTO paysim.charges (idempotency_key, id, amount, customer_id)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (idempotency_key) DO NOTHING
		RETURNING id, idempotency_key, amount, customer_id, created_at`,
		key, id, amount, customer).Scan(&c.ID, &c.Key, &c.Amount, &c.CustomerID, &c.CreatedAt)
	if err == nil {
		return c, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Charge{}, false, fmt.Errorf("insert charge: %w", err)
	}

	// The key exists. DO NOTHING waited for any concurrent insert of it to
	// commit, so this read sees the winner.
	err = s.pool.QueryRow(ctx, `
		SELECT id, idempotency_key, amount, customer_id, created_at
		FROM paysim.charges WHERE idempotency_key = $1`, key).Scan(&c.ID, &c.Key, &c.Amount, &c.CustomerID, &c.CreatedAt)
	if err != nil {
		return Charge{}, false, fmt.Errorf("read charge: %w", err)
	}
	if c.Amount != amount || c.CustomerID != customer {
		return Charge{}, false, ErrKeyReused
	}
	return c, true, nil
}

func newChargeID() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("charge id: %w", err)
	}
	return "ch_" + hex.EncodeToString(b[:]), nil
}
