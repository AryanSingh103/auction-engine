package kafkaclient_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/twmb/franz-go/pkg/kadm"

	"github.com/AryanSingh103/auction-engine/internal/kafkaclient"
	"github.com/AryanSingh103/auction-engine/internal/testkafka"
)

var server *testkafka.Server

func TestMain(m *testing.M) {
	ctx := context.Background()
	var err error
	if server, err = testkafka.Start(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "kafkaclient tests: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	_ = server.Terminate(ctx)
	os.Exit(code)
}

func TestEnsureTopicCreatesOnceAndLeavesExistingAlone(t *testing.T) {
	c, err := kafkaclient.New(server.Brokers())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := t.Context()

	if err := kafkaclient.EnsureTopic(ctx, c, "ensure-me", 3, 1); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Again, with a different partition count: no error, no change.
	if err := kafkaclient.EnsureTopic(ctx, c, "ensure-me", 6, 1); err != nil {
		t.Fatalf("second call: %v", err)
	}
	details, err := kadm.NewClient(c).ListTopics(ctx, "ensure-me")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if got := len(details["ensure-me"].Partitions); got != 3 {
		t.Errorf("partitions = %d, want the original 3", got)
	}
}

func TestEnsureTopicReportsRealErrors(t *testing.T) {
	c, err := kafkaclient.New(server.Brokers())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// A replication factor above the single broker's count must fail.
	if err := kafkaclient.EnsureTopic(t.Context(), c, "too-many-replicas", 1, 3); err == nil {
		t.Error("EnsureTopic with replication 3 on one broker returned nil")
	}
}
