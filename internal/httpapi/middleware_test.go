package httpapi

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5/middleware"
)

// logLine is the subset of the request log line these tests assert on.
type logLine struct {
	Msg       string `json:"msg"`
	Method    string `json:"method"`
	Path      string `json:"path"`
	Status    int    `json:"status"`
	Bytes     int    `json:"bytes"`
	RequestID string `json:"request_id"`
}

// serveLogged runs one request through RequestID + requestLogger + h and
// returns the single log line that must have been written.
func serveLogged(t *testing.T, method, path string, h http.Handler) logLine {
	t.Helper()
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	handler := middleware.RequestID(requestLogger(logger)(h))

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(method, path, nil))

	lines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n"))
	if len(lines) != 1 {
		t.Fatalf("got %d log lines, want exactly 1:\n%s", len(lines), buf.String())
	}
	var got logLine
	if err := json.Unmarshal(lines[0], &got); err != nil {
		t.Fatalf("log line is not JSON: %v\n%s", err, lines[0])
	}
	return got
}

func TestRequestLogger(t *testing.T) {
	tests := []struct {
		name       string
		handler    http.HandlerFunc
		wantStatus int
		wantBytes  int
	}{
		{
			name: "explicit status and body",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte("hello"))
			},
			wantStatus: http.StatusCreated,
			wantBytes:  5,
		},
		{
			name: "body without WriteHeader is an implicit 200",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("hi"))
			},
			wantStatus: http.StatusOK,
			wantBytes:  2,
		},
		{
			name:       "handler that writes nothing is logged as 200",
			handler:    func(http.ResponseWriter, *http.Request) {},
			wantStatus: http.StatusOK,
			wantBytes:  0,
		},
		{
			name:       "error status",
			handler:    func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) },
			wantStatus: http.StatusNotFound,
			wantBytes:  len("404 page not found\n"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := serveLogged(t, http.MethodPost, "/things", tt.handler)

			if got.Msg != "http request" {
				t.Errorf("msg = %q, want %q", got.Msg, "http request")
			}
			if got.Method != http.MethodPost || got.Path != "/things" {
				t.Errorf("method, path = %q, %q, want POST, /things", got.Method, got.Path)
			}
			if got.Status != tt.wantStatus {
				t.Errorf("status = %d, want %d", got.Status, tt.wantStatus)
			}
			if got.Bytes != tt.wantBytes {
				t.Errorf("bytes = %d, want %d", got.Bytes, tt.wantBytes)
			}
			if got.RequestID == "" {
				t.Error("request_id is empty; RequestID middleware value not propagated")
			}
		})
	}
}
