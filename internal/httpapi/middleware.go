package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"runtime/debug"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5/middleware"
)

// requestLogger logs one structured line per request after it completes.
//
// chi's own middleware.Logger is not used because it writes plain text through
// the standard log package, which would mix unstructured lines into our JSON
// log stream.
func requestLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			// The wrapper records the status code and byte count, which a
			// plain http.ResponseWriter does not expose after the fact.
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)

			next.ServeHTTP(ww, r)

			status := ww.Status()
			if status == 0 {
				// The handler never called WriteHeader or Write; net/http
				// sends 200 in that case, so that is what the client saw.
				status = http.StatusOK
			}
			logger.LogAttrs(r.Context(), slog.LevelInfo, "http request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", status),
				slog.Int("bytes", ww.BytesWritten()),
				// slog's JSON handler encodes a Duration as integer
				// nanoseconds, which is unreadable; milliseconds as a float
				// keeps sub-millisecond precision and is what humans and log
				// queries expect.
				slog.Float64("duration_ms", float64(time.Since(start).Microseconds())/1000),
				slog.String("request_id", middleware.GetReqID(r.Context())),
			)
		})
	}
}

// recoverer turns a panic in a handler into a 500 response and a structured
// error log with the stack trace, instead of letting it crash the connection.
//
// net/http would already recover a panicking handler, but it only prints to
// stderr in plain text and aborts the connection without a response. chi's
// middleware.Recoverer has the same plain-text problem as its Logger.
func recoverer(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hijacked := new(atomic.Bool)
			r = r.WithContext(context.WithValue(r.Context(), hijackedKey{}, hijacked))
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				// http.ErrAbortHandler is net/http's sentinel for "abort this
				// response silently"; re-panic so the server handles it as
				// designed.
				if rec == http.ErrAbortHandler { //nolint:errorlint // recover() returns the exact sentinel value, never a wrapped error
					panic(rec)
				}
				logger.LogAttrs(r.Context(), slog.LevelError, "panic recovered",
					slog.Any("panic", rec),
					slog.String("stack", string(debug.Stack())),
					slog.String("request_id", middleware.GetReqID(r.Context())),
				)
				// A WebSocket handler hijacked the connection: an HTTP status
				// can no longer be written (R12). This is a flag set by the
				// handler after a successful upgrade, not the client's
				// Upgrade header, which anyone can send to suppress the 500
				// (found by the M3 adversarial review).
				if hijacked.Load() {
					return
				}
				// If the handler already started writing a response, this
				// status cannot be sent; the client sees a truncated reply.
				w.WriteHeader(http.StatusInternalServerError)
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// requestTimeout puts a deadline on every request's context. Handlers pass
// that context to the database, so a slow query is cancelled (and answered
// with 503) before the server's WriteTimeout silently kills the connection.
// Config validation guarantees d < WriteTimeout.
//
// http.TimeoutHandler was rejected: it buffers the whole response in memory
// and breaks http.Hijacker, which WebSockets need in milestone 3.
func requestTimeout(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

type hijackedKey struct{}

// markHijacked records that the handler took over the connection, so the
// recoverer will not try to write an HTTP error on it.
func markHijacked(r *http.Request) {
	if f, ok := r.Context().Value(hijackedKey{}).(*atomic.Bool); ok {
		f.Store(true)
	}
}
