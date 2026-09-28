package httpapi

import (
	"log/slog"
	"net/http"
)

// shedBids admits at most max bid requests at once on this instance and
// answers the rest 503 immediately, with Retry-After (docs/decisions/025).
//
// Without it, excess bids queue for a database connection until their
// request deadline, so under overload every bid gets slow and then fails.
// Refusing at the door keeps latency bounded for the bids let in, and
// tells clients to back off. It runs before the rate limiter, so a shed
// request costs no Redis call.
func shedBids(max int, logger *slog.Logger, record func(shed bool)) func(http.Handler) http.Handler {
	slots := make(chan struct{}, max)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
				record(false)
				next.ServeHTTP(w, r)
			default:
				record(true)
				w.Header().Set("Retry-After", "1")
				writeError(w, logger, http.StatusServiceUnavailable, "overloaded", "server is at capacity; retry shortly")
			}
		})
	}
}
