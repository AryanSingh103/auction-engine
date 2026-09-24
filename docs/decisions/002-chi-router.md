# 002. HTTP routing with chi on net/http

## Context
The brief allows net/http with chi, or Gin. The API will grow route groups with their own middleware: rate limiting (M3), WebSocket upgrade routes (M3) and metrics (M2).

## Decision
Use chi v5 on top of `net/http`. Handlers are plain `http.HandlerFunc`, and middleware is `func(http.Handler) http.Handler`. We use chi's `RequestID` and `WrapResponseWriter`, but write our own slog request logger and panic recoverer, because chi's versions print plain text.

## Alternatives considered
- **Gin:** fast and popular, but handlers take `*gin.Context` rather than the standard types. Every handler, middleware and test is then coupled to Gin, and stdlib or third-party `http.Handler` middleware needs adapters.
- **Stdlib `ServeMux` only:** since Go 1.22 it matches methods and path wildcards, so it would work for M0. It has no route groups or per-group middleware, which we need from M3.

## Consequences
Everything stays standard `net/http`, testable with `httptest` and no framework. We still write some plumbing (JSON helpers, error responses) that Gin would provide.
