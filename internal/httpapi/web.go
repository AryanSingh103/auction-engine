package httpapi

import (
	_ "embed"
	"net/http"
)

// The one-page, deliberately plain frontend, compiled into the binary.
//
//go:embed web/index.html
var indexHTML []byte

func handleIndex(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(indexHTML)
}
