// Package testredis gives integration tests a real Redis, one container per
// test binary (started from TestMain), like internal/testdb does for
// Postgres.
package testredis

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"

	"github.com/AryanSingh103/auction-engine/internal/redisclient"
)

// Image must match the redis service in compose.yaml.
const Image = "redis:8.8.3-alpine3.23"

// Server is a running Redis container.
type Server struct {
	container *tcredis.RedisContainer
	url       string
}

// Start launches the container. Call it once from TestMain.
func Start(ctx context.Context) (*Server, error) {
	c, err := tcredis.Run(ctx, Image)
	if err != nil {
		return nil, fmt.Errorf("start redis container: %w", err)
	}
	url, err := c.ConnectionString(ctx)
	if err != nil {
		_ = testcontainers.TerminateContainer(c)
		return nil, fmt.Errorf("redis connection string: %w", err)
	}
	return &Server{container: c, url: url}, nil
}

// URL is the container's redis:// URL.
func (s *Server) URL() string { return s.url }

// Terminate stops and removes the container.
func (s *Server) Terminate(ctx context.Context) error {
	return testcontainers.TerminateContainer(s.container, testcontainers.StopContext(ctx))
}

// NewClient returns a client on an emptied database. Tests using it must
// not run in parallel with each other within one package.
func (s *Server) NewClient(t testing.TB) *redis.Client {
	t.Helper()
	c, err := redisclient.New(s.url, 2*time.Second)
	if err != nil {
		t.Fatalf("redis client: %v", err)
	}
	if err := c.FlushDB(t.Context()).Err(); err != nil {
		t.Fatalf("flush redis: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}
