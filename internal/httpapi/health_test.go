package httpapi

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

var discardLogger = slog.New(slog.DiscardHandler)

// These tests go through NewRouter rather than calling handleHealthz
// directly, so they also cover the route registration and method matching.

func TestHealthz(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/healthz", nil)

	NewRouter(discardLogger).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want %q", got, "application/json")
	}
	if got, want := rec.Body.String(), `{"status":"ok"}`+"\n"; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

func TestHealthzRejectsOtherMethods(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(t.Context(), method, "/healthz", nil)

			NewRouter(discardLogger).ServeHTTP(rec, req)

			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
			}
		})
	}
}

func TestUnknownPathIs404(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/nope", nil)

	NewRouter(discardLogger).ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}
