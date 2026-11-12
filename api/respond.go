package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/SpliceFS/common/wire"
)

const (
	jsonType         = "application/json"
	internalErrorMsg = "internal error"
)

// writeError sends status and a wire.Error body.
func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", jsonType)
	w.WriteHeader(status)
	// The client may be gone, and there is nobody to tell.
	_ = json.NewEncoder(w).Encode(wire.Error{Error: message})
}

// internalError logs err and answers 500. The client learns nothing about the cause.
func (s *server) internalError(w http.ResponseWriter, r *http.Request, err error) {
	s.log.ErrorContext(r.Context(), "request failed",
		"method", r.Method, "path", r.URL.Path, "client", clientAddr(r.Context()), "error", err)
	writeError(w, http.StatusInternalServerError, internalErrorMsg)
}

// writeJSON sends v as the body of a 200 response. A body of at least minCompressSize
// bytes is compressed when the client accepts gzip.
func (s *server) writeJSON(w http.ResponseWriter, r *http.Request, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		s.internalError(w, r, fmt.Errorf("encode response: %w", err))
		return
	}
	h := w.Header()
	h.Set("Content-Type", jsonType)
	h.Add("Vary", "Accept-Encoding")
	if len(body) >= minCompressSize && acceptsGzip(r.Header) {
		// The source is memory, so the only failure is a client that went away.
		_ = writeGzip(w, bytes.NewReader(body))
		return
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// errorBodyWriter replaces the plain text error bodies that http.ServeContent writes
// with wire.Error JSON, so that every error response has the same shape.
type errorBodyWriter struct {
	http.ResponseWriter
	replaced bool
}

// WriteHeader sends a wire.Error body of its own for any status of 400 or more.
func (w *errorBodyWriter) WriteHeader(status int) {
	if status < http.StatusBadRequest {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	w.replaced = true
	w.Header().Del("Content-Length")
	writeError(w.ResponseWriter, status, errorMessage(status))
}

// Write discards the body that follows an error status.
func (w *errorBodyWriter) Write(p []byte) (int, error) {
	if w.replaced {
		return len(p), nil
	}
	return w.ResponseWriter.Write(p)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *errorBodyWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// errorMessage is the message of an error that the standard library generates.
func errorMessage(status int) string {
	if status >= http.StatusInternalServerError {
		return internalErrorMsg
	}
	return strings.ToLower(http.StatusText(status))
}
