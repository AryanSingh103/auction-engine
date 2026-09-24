package testredis

import (
	"context"
	"fmt"
	"os"
	"testing"
)

var server *Server

func TestMain(m *testing.M) {
	ctx := context.Background()
	var err error
	server, err = Start(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "testredis: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	_ = server.Terminate(ctx)
	os.Exit(code)
}

func TestNewClientStartsEmpty(t *testing.T) {
	c := server.NewClient(t)
	if err := c.Set(t.Context(), "k", "v", 0).Err(); err != nil {
		t.Fatalf("set: %v", err)
	}
	c2 := server.NewClient(t) // flushes
	if n, err := c2.DBSize(t.Context()).Result(); err != nil || n != 0 {
		t.Errorf("DBSize after NewClient = %d, %v; want 0", n, err)
	}
}
