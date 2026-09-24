package httpapi

import "net/http"

// handleHealthz is a liveness probe: it answers "the process is up and serving
// HTTP". It deliberately does not check dependencies such as Postgres. A
// liveness probe that fails when the database is down would make an
// orchestrator restart healthy API processes in a loop, which cannot fix a
// database outage. Dependency checks belong on a readiness endpoint (/readyz,
// milestone 1).
func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	// A failed write means the client went away; there is nobody to report to.
	_, _ = w.Write([]byte(`{"status":"ok"}` + "\n"))
}
