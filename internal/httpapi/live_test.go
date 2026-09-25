package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AryanSingh103/auction-engine/internal/auction"
	"github.com/AryanSingh103/auction-engine/internal/live"
)

// instance is one API process: its own hub, subscribed to the shared Redis,
// serving HTTP with deliberately hostile timeouts.
type instance struct {
	srv *httptest.Server
	hub *live.Hub
	svc *auction.Service
}

func startInstance(t *testing.T, pool *pgxpool.Pool, bus *live.Bus) *instance {
	t.Helper()
	hub := live.NewHub(16, nil)
	ready, done := bus.Forward(t.Context(), hub)
	<-ready
	svc := auction.NewService(pool, auction.WithPublisher(bus))
	srv := httptest.NewUnstartedServer(NewRouter(Options{
		Logger: discardLogger, Auctions: svc, Ready: pool.Ping,
		// Both far shorter than the test: the live route must be immune to
		// the request deadline, and the handler must clear the server's
		// write deadline.
		RequestTimeout: 200 * time.Millisecond,
		Live:           &LiveOptions{Hub: hub, PingInterval: time.Second, WriteTimeout: time.Second},
	}))
	srv.Config.WriteTimeout = 300 * time.Millisecond
	srv.Config.ReadTimeout = 300 * time.Millisecond
	srv.Start()
	t.Cleanup(func() {
		hub.Close()
		srv.Close()
		<-done
	})
	return &instance{srv: srv, hub: hub, svc: svc}
}

func seedAuction(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO users (name) VALUES ('alice'), ('bob');
		INSERT INTO items (title) VALUES ('unit');
		INSERT INTO auctions (item_id, start_at, end_at, starting_price, min_increment)
		VALUES (1, clock_timestamp() - interval '1 minute', clock_timestamp() + interval '1 hour', 1000, 100);`); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func dialLive(t *testing.T, srv *httptest.Server, auctionID string) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/auctions/" + auctionID + "/live"
	conn, resp, err := websocket.Dial(t.Context(), url, nil)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("dial %s: %v", url, err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	return conn
}

// readType reads messages until one of the wanted type arrives.
func readType(t *testing.T, conn *websocket.Conn, want string, into any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("waiting for %q message: %v", want, err)
		}
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(data, &head); err != nil {
			t.Fatalf("message is not JSON: %s", data)
		}
		if head.Type == want {
			if err := json.Unmarshal(data, into); err != nil {
				t.Fatalf("decode %s: %v", want, err)
			}
			return
		}
	}
}

func postBid(t *testing.T, srv *httptest.Server, user, key string, amount int) {
	t.Helper()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/auctions/1/bids",
		strings.NewReader(`{"amount": `+strconv.Itoa(amount)+`}`))
	req.Header.Set("X-User-ID", user)
	req.Header.Set("Idempotency-Key", key)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("bid status %d, want 201", resp.StatusCode)
	}
}

// A client connected to instance A receives bids placed through instance B
// (Redis pub/sub), after surviving longer than both the request deadline
// and the server's write timeout; the bid chain lets it verify it missed
// nothing.
func TestLiveStreamAcrossInstances(t *testing.T) {
	pool := server.NewDB(t, 8)
	seedAuction(t, pool)
	bus := live.NewBus(redisServer.NewClient(t), nil)
	a, b := startInstance(t, pool, bus), startInstance(t, pool, bus)

	conn := dialLive(t, a.srv, "1")
	var snap snapshotMessage
	readType(t, conn, live.TypeSnapshot, &snap)
	if snap.Auction.ID != 1 || snap.Auction.CurrentBidID != nil {
		t.Fatalf("snapshot = %+v, want auction 1 with no bids", snap.Auction)
	}

	time.Sleep(700 * time.Millisecond) // > RequestTimeout and server WriteTimeout

	postBid(t, b.srv, "1", "k1", 1000)
	var first live.BidMessage
	readType(t, conn, live.TypeBid, &first)
	if first.Bid.Amount != 1000 || first.Bid.PrevBidID != nil {
		t.Fatalf("first bid message = %+v", first.Bid)
	}

	postBid(t, b.srv, "2", "k2", 1100)
	var second live.BidMessage
	readType(t, conn, live.TypeBid, &second)
	if second.Bid.PrevBidID == nil || *second.Bid.PrevBidID != first.Bid.ID {
		t.Errorf("second bid prev = %v, want %d: the chain lets clients detect gaps", second.Bid.PrevBidID, first.Bid.ID)
	}
}

func TestLiveUnknownAuctionIs404(t *testing.T) {
	pool := server.NewDB(t, 4)
	a := startInstance(t, pool, live.NewBus(redisServer.NewClient(t), nil))
	url := "ws" + strings.TrimPrefix(a.srv.URL, "http") + "/auctions/99/live"
	_, resp, err := websocket.Dial(t.Context(), url, nil)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err == nil || resp == nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("dial unknown auction: err %v, resp %v; want a 404 before upgrade", err, resp)
	}
}

// On shutdown the server closes every stream with 1001 Going Away, so
// clients reconnect elsewhere (http.Server.Shutdown alone would not: it
// does not track hijacked connections).
func TestLiveShutdownClosesWithGoingAway(t *testing.T) {
	pool := server.NewDB(t, 4)
	seedAuction(t, pool)
	a := startInstance(t, pool, live.NewBus(redisServer.NewClient(t), nil))
	conn := dialLive(t, a.srv, "1")
	var snap snapshotMessage
	readType(t, conn, live.TypeSnapshot, &snap)

	a.hub.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	_, _, err := conn.Read(ctx)
	if status := websocket.CloseStatus(err); status != websocket.StatusGoingAway {
		t.Fatalf("read after shutdown: err %v (status %d), want close 1001", err, status)
	}
}

// The periodic sync lets a client that missed the last bid (lost pub/sub
// message) notice: the sync's head differs from the client's.
func TestLiveSyncBroadcastsCurrentHead(t *testing.T) {
	pool := server.NewDB(t, 4)
	seedAuction(t, pool)
	// No bus subscription on this instance: the bid's pub/sub message is
	// never delivered, exactly the loss the sync must cover.
	hub := live.NewHub(16, nil)
	svc := auction.NewService(pool)
	srv := httptest.NewServer(NewRouter(Options{
		Logger: discardLogger, Auctions: svc, Ready: pool.Ping, RequestTimeout: time.Second,
		Live: &LiveOptions{Hub: hub, PingInterval: time.Second, WriteTimeout: time.Second},
	}))
	defer srv.Close()
	defer hub.Close()

	conn := dialLive(t, srv, "1")
	var snap snapshotMessage
	readType(t, conn, live.TypeSnapshot, &snap)

	bid, _, err := svc.PlaceBid(t.Context(), auction.PlaceBidRequest{AuctionID: 1, UserID: 1, Amount: 1000, IdempotencyKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	RunLiveSync(ctx, svc, hub, 50*time.Millisecond, discardLogger)

	var sync syncMessage
	readType(t, conn, live.TypeSync, &sync)
	if sync.HeadBidID == nil || *sync.HeadBidID != bid.ID {
		t.Errorf("sync head = %v, want %d", sync.HeadBidID, bid.ID)
	}
}

// staleCache always serves the state it was created with.
type staleCache struct{ a auction.Auction }

func (c staleCache) Get(context.Context, int64) (auction.Auction, bool, error) { return c.a, true, nil }
func (c staleCache) Put(context.Context, auction.Auction) error                { return nil }

// A cache left stale (a lost post-commit refresh) must not make the sync
// repeat the stale head: sync reads Postgres.
func TestLiveSyncIgnoresStaleCache(t *testing.T) {
	pool := server.NewDB(t, 4)
	seedAuction(t, pool)
	before, err := auction.NewService(pool).GetAuction(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	svc := auction.NewService(pool, auction.WithCache(staleCache{before}))
	bid, _, err := svc.PlaceBid(t.Context(), auction.PlaceBidRequest{AuctionID: 1, UserID: 1, Amount: 1000, IdempotencyKey: "k"})
	if err != nil {
		t.Fatal(err)
	}

	hub := live.NewHub(4, nil)
	sub, _ := hub.Join(1)
	ctx, cancel := context.WithCancel(t.Context())
	done := RunLiveSync(ctx, svc, hub, 20*time.Millisecond, discardLogger)
	var m syncMessage
	select {
	case raw := <-sub.Send:
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no sync message")
	}
	cancel()
	<-done
	if m.HeadBidID == nil || *m.HeadBidID != bid.ID {
		t.Errorf("sync head = %v, want %d (Postgres), not the stale cached head", m.HeadBidID, bid.ID)
	}
}
