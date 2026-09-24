package httpapi

import (
	"bytes"
	"context"
	"errors"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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
	// Pointer so a missing field is distinguishable from 0.
	DurationMS *float64 `json:"duration_ms"`
}

// serveLogged runs one request through RequestID + requestLogger + h and
// returns the single log line that must have been written.
func serveLogged(t *testing.T, method, path string, h http.Handler) logLine {
	t.Helper()
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	handler := middleware.RequestID(requestLogger(logger)(h))

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), method, path, nil))

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
			if got.DurationMS == nil || *got.DurationMS < 0 {
				t.Errorf("duration_ms = %v, want a non-negative number", got.DurationMS)
			}
		})
	}
}

func TestRecovererTurnsPanicInto500(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	// Same order as NewRouter: the logger must sit outside the recoverer to
	// see the 500.
	handler := middleware.RequestID(requestLogger(logger)(recoverer(logger)(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }),
	)))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/explode", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("response status = %d, want 500", rec.Code)
	}

	lines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("got %d log lines, want 2 (panic, then request):\n%s", len(lines), buf.String())
	}
	var panicLine struct {
		Level     string `json:"level"`
		Msg       string `json:"msg"`
		Panic     string `json:"panic"`
		Stack     string `json:"stack"`
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(lines[0], &panicLine); err != nil {
		t.Fatalf("panic log line is not JSON: %v", err)
	}
	if panicLine.Level != "ERROR" || panicLine.Msg != "panic recovered" || panicLine.Panic != "boom" {
		t.Errorf("panic line = %+v, want level ERROR, msg %q, panic %q", panicLine, "panic recovered", "boom")
	}
	if !bytes.Contains([]byte(panicLine.Stack), []byte("TestRecovererTurnsPanicInto500")) {
		t.Errorf("stack does not include the panicking frame:\n%s", panicLine.Stack)
	}
	if panicLine.RequestID == "" {
		t.Error("panic line has empty request_id")
	}

	var reqLine logLine
	if err := json.Unmarshal(lines[1], &reqLine); err != nil {
		t.Fatalf("request log line is not JSON: %v", err)
	}
	if reqLine.Status != http.StatusInternalServerError {
		t.Errorf("request log status = %d, want 500", reqLine.Status)
	}
	if reqLine.RequestID != panicLine.RequestID {
		t.Errorf("request_id differs between lines: %q vs %q", reqLine.RequestID, panicLine.RequestID)
	}
}

func TestRecovererRepanicsErrAbortHandler(t *testing.T) {
	handler := recoverer(discardLogger)(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) }),
	)

	defer func() {
		if got := recover(); got != http.ErrAbortHandler { //nolint:errorlint // comparing the exact recovered sentinel
			t.Errorf("recovered %v, want http.ErrAbortHandler to propagate", got)
		}
	}()
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
	t.Error("ServeHTTP returned normally; want the ErrAbortHandler panic to propagate")
}

func TestRequestTimeoutSetsDeadline(t *testing.T) {
	var deadline time.Time
	var hasDeadline bool
	var ctxErrAfter error
	h := requestTimeout(50 * time.Millisecond)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		deadline, hasDeadline = r.Context().Deadline()
		<-r.Context().Done() // a handler stuck on a slow query
		ctxErrAfter = r.Context().Err()
	}))

	start := time.Now()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))

	if !hasDeadline {
		t.Fatal("request context has no deadline")
	}
	if got := deadline.Sub(start); got > 60*time.Millisecond {
		t.Errorf("deadline is %s after start, want about 50ms", got)
	}
	if !errors.Is(ctxErrAfter, context.DeadlineExceeded) {
		t.Errorf("context error = %v, want DeadlineExceeded", ctxErrAfter)
	}
}
