package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestReadyz(t *testing.T) {
	tests := []struct {
		name  string
		ready func(context.Context) error
		want  int
	}{
		{"dependency up", func(context.Context) error { return nil }, http.StatusOK},
		{"dependency down", func(context.Context) error { return errors.New("connection refused") }, http.StatusServiceUnavailable},
		{"dependency hangs", func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }, http.StatusServiceUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewRouter(Options{Logger: discardLogger, Ready: tt.ready, RequestTimeout: 5 * time.Second})
			rec := httptest.NewRecorder()
			start := time.Now()

			h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", nil))

			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d; body %s", rec.Code, tt.want, rec.Body)
			}
			if elapsed := time.Since(start); elapsed > readyTimeout+500*time.Millisecond {
				t.Errorf("took %s; the readiness check is not bounded by readyTimeout", elapsed)
			}
		})
	}
}
