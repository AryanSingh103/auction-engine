package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"github.com/AryanSingh103/auction-engine/internal/auction"
	"github.com/AryanSingh103/auction-engine/internal/live"
)

// LiveOptions configure the WebSocket endpoint.
type LiveOptions struct {
	Hub *live.Hub
	// PingInterval: how often each connection is pinged. It must be well
	// under any proxy idle timeout (the ALB's is 60s) and detects dead
	// clients whose TCP connection never closed.
	PingInterval time.Duration
	// WriteTimeout bounds each message write and ping.
	WriteTimeout time.Duration
	// OnConnections, if set, receives +1/-1 as connections open and close.
	OnConnections func(delta int)
}

type snapshotMessage struct {
	Type    string          `json:"type"`
	Auction auctionResponse `json:"auction"`
}

type syncMessage struct {
	Type         string `json:"type"`
	AuctionID    int64  `json:"auction_id"`
	HeadBidID    *int64 `json:"head_bid_id"`
	CurrentPrice *int64 `json:"current_price"`
}

// handleAuctionLive serves GET /auctions/{auctionID}/live: a WebSocket that
// first sends a snapshot of the auction, then every accepted bid (from any
// API instance), plus a periodic sync of the current head.
//
// Client contract (docs/decisions/020): apply a bid only if its prev_bid_id
// equals your current head bid; ignore bids at or below your head; on any
// other mismatch, or a sync whose head differs from yours, re-read the
// auction (GET /auctions/{id}). Messages can be lost (pub/sub), and a
// client dropped for being slow must reconnect.
func handleAuctionLive(svc *auction.Service, o LiveOptions, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(r, "auctionID")
		if !ok {
			writeError(w, logger, http.StatusNotFound, "auction_not_found", "auction not found")
			return
		}
		// 404 before upgrading, as a normal HTTP response.
		if _, err := svc.GetAuction(r.Context(), id); err != nil {
			writeServiceError(w, r, logger, err)
			return
		}

		// The server's ReadTimeout/WriteTimeout do NOT need clearing here:
		// Accept hijacks the connection, and net/http clears its deadlines
		// on hijack (net/http/server.go, conn.hijackLocked). A mutation test
		// removing an explicit clear showed the stream survives without it.
		// (They would matter for a non-hijacked stream such as SSE.)
		// What does need care is the request deadline middleware, which is
		// why this route sits outside it (see NewRouter).
		//
		// Default options: cross-origin browser connections are refused
		// (the page is served by this API, so it is same-origin).
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return // Accept has already written the HTTP error
		}
		markHijacked(r)
		defer func() { _ = conn.CloseNow() }()

		// Join BEFORE reading the snapshot: bids accepted in between are
		// queued, not missed. Queued bids at or below the snapshot's head
		// are ignored by the client contract.
		sub, err := o.Hub.Join(id)
		if err != nil {
			_ = conn.Close(websocket.StatusGoingAway, "server shutting down")
			return
		}
		defer o.Hub.Leave(sub)
		if o.OnConnections != nil {
			o.OnConnections(1)
			defer o.OnConnections(-1)
		}

		// We never read client messages; CloseRead handles control frames
		// and cancels ctx when the client goes away.
		ctx := conn.CloseRead(r.Context())

		a, err := svc.GetAuction(ctx, id)
		if err != nil {
			_ = conn.Close(websocket.StatusInternalError, "could not load auction")
			return
		}
		snapshot, _ := json.Marshal(snapshotMessage{Type: live.TypeSnapshot, Auction: toAuctionResponse(a)})
		if err := write(ctx, conn, snapshot, o.WriteTimeout); err != nil {
			return
		}

		ping := time.NewTicker(o.PingInterval)
		defer ping.Stop()
		for {
			select {
			case msg := <-sub.Send:
				if err := write(ctx, conn, msg, o.WriteTimeout); err != nil {
					return
				}
			case <-sub.Dropped():
				if sub.Reason() == live.ReasonSlow {
					// 1013 Try Again Later: reconnect and resync.
					_ = conn.Close(websocket.StatusTryAgainLater, "too slow; reconnect")
				} else {
					_ = conn.Close(websocket.StatusGoingAway, "server shutting down")
				}
				return
			case <-ping.C:
				pctx, cancel := context.WithTimeout(ctx, o.WriteTimeout)
				err := conn.Ping(pctx)
				cancel()
				if err != nil {
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}
}

func write(ctx context.Context, conn *websocket.Conn, msg []byte, timeout time.Duration) error {
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return conn.Write(wctx, websocket.MessageText, msg)
}

// RunLiveSync broadcasts every room's current head every interval until ctx
// is cancelled. Pub/sub can lose a message; without this, a client that
// missed the LAST bid of a quiet auction would show a stale price forever.
//
// It reads Postgres directly, not through the cache: a cache left stale by a
// failed refresh would otherwise make the sync repeat the stale head, and
// the staleness bound would become TTL + interval instead of interval.
// Found by the M3 adversarial review. Each read has its own deadline so one
// slow room cannot stall the others. done is closed when it returns.
func RunLiveSync(ctx context.Context, svc *auction.Service, hub *live.Hub, interval time.Duration, logger *slog.Logger) (done <-chan struct{}) {
	d := make(chan struct{})
	go func() {
		defer close(d)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			for _, id := range hub.Rooms() {
				rctx, cancel := context.WithTimeout(ctx, interval)
				a, err := svc.ReadAuction(rctx, id)
				cancel()
				if err != nil {
					if ctx.Err() == nil {
						logger.WarnContext(ctx, "live sync: read auction", slog.Int64("auction_id", id), slog.Any("error", err))
					}
					continue
				}
				m := syncMessage{Type: live.TypeSync, AuctionID: id}
				if a.Head != nil {
					m.HeadBidID, m.CurrentPrice = &a.Head.BidID, &a.Head.Price
				}
				msg, _ := json.Marshal(m)
				hub.Broadcast(id, msg)
			}
		}
	}()
	return d
}
