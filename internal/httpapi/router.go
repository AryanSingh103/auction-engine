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
}

// NewRouter returns the API's root handler.
//
// Middleware order matters: RequestID runs first so every later middleware
// and handler can read the ID from the request context, and the request
// logger wraps everything after it so it sees the final status code,
// including the 500 written by recoverer, which must sit inside the logger.
// The request deadline is innermost so its clock starts as close to the
// handler as possible.
func NewRouter(o Options) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(requestLogger(o.Logger))
	r.Use(recoverer(o.Logger))
	r.Use(requestTimeout(o.RequestTimeout))

	r.Get("/healthz", handleHealthz)
	r.Get("/readyz", handleReadyz(o.Ready, o.Logger))

	return r
}
