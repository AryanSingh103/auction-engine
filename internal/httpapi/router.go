// Package httpapi wires HTTP routes and middleware for the API process.
package httpapi

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// NewRouter returns the API's root handler.
//
// Middleware order matters: RequestID runs first so every later middleware
// and handler can read the ID from the request context, and the request
// logger wraps everything after it so it sees the final status code,
// including the 500 written by recoverer, which must sit inside the logger.
func NewRouter(logger *slog.Logger) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(requestLogger(logger))
	r.Use(recoverer(logger))

	r.Get("/healthz", handleHealthz)

	return r
}
