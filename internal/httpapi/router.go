// Package httpapi wires HTTP routes and middleware for the API process.
package httpapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// NewRouter returns the API's root handler.
//
// Middleware order matters: RequestID runs first so every later middleware
// and handler can read the ID from the request context.
func NewRouter() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)

	r.Get("/healthz", handleHealthz)

	return r
}
