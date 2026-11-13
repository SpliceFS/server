package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
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
	//nolint:errcheck,errchkjson // The status is sent already; a failed body write has no other recipient.
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
	header := w.Header()
	header.Set("Content-Type", jsonType)
	header.Add("Vary", "Accept-Encoding")
	if len(body) >= minCompressSize && acceptsGzip(r.Header) {
		s.sendGzip(w, r, bytes.NewReader(body))
		return
	}
	header.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	//nolint:errcheck // The status is sent already; a client that went away is not told anything.
	_, _ = w.Write(body)
}

// sendGzip sends src with gzip. The status is sent before the content, so a failure can only
// be logged; for a source in memory it means that the client went away.
func (s *server) sendGzip(w http.ResponseWriter, r *http.Request, src io.Reader) {
	if err := writeGzip(w, src); err != nil {
		s.log.DebugContext(r.Context(), "compressed response ended early",
			"path", r.URL.Path, "error", err)
	}
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
	n, err := w.ResponseWriter.Write(p)
	if err != nil {
		return n, fmt.Errorf("write response body: %w", err)
	}
	return n, nil
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
