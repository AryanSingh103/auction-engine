package httpapi

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"

	"github.com/AryanSingh103/auction-engine/internal/ratelimit"
)

// BidLimiter decides whether a user may place another bid now.
type BidLimiter interface {
	Allow(ctx context.Context, key string) (ratelimit.Decision, error)
}

// rateLimitBids applies the per-user bid token bucket. It answers 429 with
// Retry-After when the bucket is empty.
//
// It FAILS OPEN: if Redis is slow or down, the bid proceeds. The rate limit
// protects capacity, not correctness; every invariant is enforced by
// Postgres, so letting bids through during a Redis outage costs load, while
// refusing them would turn a cache outage into a full outage. See
// docs/decisions/018.
//
// The key is the claimed X-User-ID. There is no authentication
// (docs/decisions/010), so a client can spread its bids over many ids; this
// limits honest clients' mistakes, not a determined attacker.
func rateLimitBids(l BidLimiter, logger *slog.Logger, record func(result string)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user, err := strconv.ParseInt(r.Header.Get("X-User-ID"), 10, 64)
			if err != nil || user <= 0 {
				next.ServeHTTP(w, r) // the handler answers 401
				return
			}
			d, err := l.Allow(r.Context(), "bid:user:"+strconv.FormatInt(user, 10))
			switch {
			case err != nil:
				record("error")
				logger.WarnContext(r.Context(), "rate limiter unavailable; allowing request", slog.Any("error", err))
				next.ServeHTTP(w, r)
			case !d.Allowed:
				record("limited")
				// Retry-After is whole seconds (RFC 9110); round up so a
				// client that obeys it never retries too early.
				secs := max(1, int(math.Ceil(d.RetryAfter.Seconds())))
				w.Header().Set("Retry-After", strconv.Itoa(secs))
				writeError(w, logger, http.StatusTooManyRequests, "rate_limited",
					fmt.Sprintf("too many bids; retry in %s", d.RetryAfter))
			default:
				record("allowed")
				next.ServeHTTP(w, r)
			}
		})
	}
}
