// Package testkafka gives integration tests a real Kafka API (Redpanda),
// one container per test binary (started from TestMain), like
// internal/testdb does for Postgres and internal/testredis for Redis.
package testkafka

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcredpanda "github.com/testcontainers/testcontainers-go/modules/redpanda"
	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"
)

// Image must match the redpanda service in compose.yaml.
const Image = "docker.redpanda.com/redpandadata/redpanda:v26.2.3"

// Server is a running Redpanda container.
type Server struct {
	container *tcredpanda.Container
	broker    string
	topics    atomic.Int64
}

// Start launches the container. Call it once from TestMain.
func Start(ctx context.Context) (*Server, error) {
	c, err := tcredpanda.Run(ctx, Image)
	if err != nil {
		return nil, fmt.Errorf("start redpanda container: %w", err)
	}
	broker, err := c.KafkaSeedBroker(ctx)
	if err != nil {
		_ = testcontainers.TerminateContainer(c)
		return nil, fmt.Errorf("redpanda seed broker: %w", err)
	}
	return &Server{container: c, broker: broker}, nil
}

// Brokers is the seed broker list for clients.
func (s *Server) Brokers() []string { return []string{s.broker} }

// Terminate stops and removes the container.
func (s *Server) Terminate(ctx context.Context) error {
	return testcontainers.TerminateContainer(s.container, testcontainers.StopContext(ctx))
}

// NewTopic creates a topic no other test uses, so tests never see each
// other's records, and returns its name.
func (s *Server) NewTopic(t testing.TB, partitions int32) string {
	t.Helper()
	name := fmt.Sprintf("%s-%d", sanitize(t.Name()), s.topics.Add(1))
	adm := kadm.NewClient(s.NewClient(t))
	resp, err := adm.CreateTopic(t.Context(), partitions, 1, nil, name)
	if err != nil {
		t.Fatalf("create topic %s: %v", name, err)
	}
	if resp.Err != nil {
		t.Fatalf("create topic %s: %v", name, resp.Err)
	}
	return name
}

// NewClient returns a client connected to the container, closed when the
// test ends.
func (s *Server) NewClient(t testing.TB, opts ...kgo.Opt) *kgo.Client {
	t.Helper()
	c, err := kgo.NewClient(append([]kgo.Opt{kgo.SeedBrokers(s.broker)}, opts...)...)
	if err != nil {
		t.Fatalf("kafka client: %v", err)
	}
	t.Cleanup(c.Close)
	return c
}

// ReadAll consumes topic from the start until it has n records or the
// timeout passes, and returns what it got.
func (s *Server) ReadAll(t testing.TB, topic string, n int, timeout time.Duration) []*kgo.Record {
	t.Helper()
	c := s.NewClient(t, kgo.ConsumeTopics(topic), kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))
	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()
	var out []*kgo.Record
	for len(out) < n {
		fetches := c.PollFetches(ctx)
		if ctx.Err() != nil {
			break
		}
		if errs := fetches.Errors(); len(errs) > 0 {
			t.Fatalf("fetch %s: %v", topic, errs)
		}
		out = append(out, fetches.Records()...)
	}
	return out
}

// sanitize turns a test name into a legal topic name.
func sanitize(name string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		}
		return '_'
	}, name)
}
