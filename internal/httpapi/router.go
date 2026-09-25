// Package httpapi wires HTTP routes and middleware for the API process.
package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/AryanSingh103/auction-engine/internal/auction"
)

// Options are the router's dependencies.
type Options struct {
	Logger *slog.Logger
	// Auctions serves the auction and bid endpoints.
	Auctions *auction.Service
	// Ready reports whether the process can serve traffic, e.g. by pinging
	// the database. It backs /readyz.
	Ready func(ctx context.Context) error
	// RequestTimeout is the deadline put on every request context.
	RequestTimeout time.Duration
	// Metrics, if set, instruments every request (see internal/metrics).
	Metrics func(http.Handler) http.Handler
	// BidLimiter, if set, rate-limits bid placement per user.
	BidLimiter BidLimiter
	// Live, if set, enables GET /auctions/{auctionID}/live (WebSocket).
	Live *LiveOptions
	// RecordRateLimit receives each limiter decision ("allowed", "limited",
	// "error") for metrics. Optional.
	RecordRateLimit func(result string)
}

// NewRouter returns the API's root handler.
//
// Middleware order matters: RequestID runs first so every later middleware
// and handler can read the ID from the request context, and the request
// logger wraps everything after it so it sees the final status code,
// including the 500 written by recoverer, which must sit inside the logger.
// The request deadline is innermost so its clock starts as close to the
// handler as possible, and it does not apply to the WebSocket route.
func NewRouter(o Options) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(requestLogger(o.Logger))
	if o.Metrics != nil {
		// Outside recoverer, like the logger, so panics count as 500s.
		r.Use(o.Metrics)
	}
	r.Use(recoverer(o.Logger))

	// Long-lived WebSocket streams live outside the request deadline group:
	// REQUEST_TIMEOUT would cancel them after a few seconds.
	if o.Live != nil {
		r.Get("/auctions/{auctionID}/live", handleAuctionLive(o.Auctions, *o.Live, o.Logger))
	}

	r.Group(func(r chi.Router) {
		r.Use(requestTimeout(o.RequestTimeout))

		r.Get("/healthz", handleHealthz)
		r.Get("/readyz", handleReadyz(o.Ready, o.Logger))
		r.Get("/auctions/{auctionID}", handleGetAuction(o.Auctions, o.Logger))

		r.Group(func(r chi.Router) {
			// Only bid placement is rate limited; reads are served from
			// cache and cheap, and limiting them would hide the auction
			// from bidders.
			if o.BidLimiter != nil {
				record := o.RecordRateLimit
				if record == nil {
					record = func(string) {}
				}
				r.Use(rateLimitBids(o.BidLimiter, o.Logger, record))
			}
			r.Post("/auctions/{auctionID}/bids", handlePlaceBid(o.Auctions, o.Logger))
		})
	})

	return r
}
