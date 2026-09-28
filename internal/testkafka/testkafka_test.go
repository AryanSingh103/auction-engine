package testkafka

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
)

var server *Server

func TestMain(m *testing.M) {
	ctx := context.Background()
	var err error
	server, err = Start(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "testkafka: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	_ = server.Terminate(ctx)
	os.Exit(code)
}

func TestProduceAndReadBack(t *testing.T) {
	topic := server.NewTopic(t, 3)
	c := server.NewClient(t)
	for i := range 5 {
		r := &kgo.Record{Topic: topic, Key: []byte("k"), Value: fmt.Appendf(nil, "v%d", i)}
		if err := c.ProduceSync(t.Context(), r).FirstErr(); err != nil {
			t.Fatalf("produce: %v", err)
		}
	}
	got := server.ReadAll(t, topic, 5, 10*time.Second)
	if len(got) != 5 {
		t.Fatalf("read %d records, want 5", len(got))
	}
	for i, r := range got { // one key, so one partition, so produce order
		if want := fmt.Sprintf("v%d", i); string(r.Value) != want {
			t.Errorf("record %d = %q, want %q", i, r.Value, want)
		}
	}
}

func TestTopicsAreIsolated(t *testing.T) {
	if a, b := server.NewTopic(t, 1), server.NewTopic(t, 1); a == b {
		t.Fatalf("two NewTopic calls returned the same name %q", a)
	}
}
