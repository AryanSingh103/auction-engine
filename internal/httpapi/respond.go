package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// errorBody is the JSON shape of every error response. Code is stable and
// machine-readable; Message is for humans and may change.
type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		// Minimum is set for bid_too_low: the smallest acceptable bid.
		Minimum *int64 `json:"minimum,omitempty"`
	} `json:"error"`
}

func writeJSON(w http.ResponseWriter, logger *slog.Logger, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// Headers are already sent; all we can do is record it.
		logger.Warn("write response body", slog.Any("error", err))
	}
}

func writeError(w http.ResponseWriter, logger *slog.Logger, status int, code, message string) {
	var body errorBody
	body.Error.Code = code
	body.Error.Message = message
	writeJSON(w, logger, status, body)
}
