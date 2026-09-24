package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"
)

// readyTimeout bounds the dependency check so a hung database makes /readyz
// fail fast instead of piling up probe requests.
const readyTimeout = 2 * time.Second

// handleReadyz is the readiness probe: "can this process serve real
// traffic right now?" Unlike /healthz it checks dependencies, so a load
// balancer stops routing here while the database is unreachable, without
// restarting a process that is otherwise fine.
func handleReadyz(ready func(context.Context) error, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), readyTimeout)
		defer cancel()
		if err := ready(ctx); err != nil {
			logger.WarnContext(r.Context(), "readiness check failed", slog.Any("error", err))
			writeError(w, logger, http.StatusServiceUnavailable, "not_ready", "a dependency is unavailable")
			return
		}
		writeJSON(w, logger, http.StatusOK, map[string]string{"status": "ready"})
	}
}
