package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"runtime/debug"
	"slices"
	"strings"
	"time"
)

// contextKey is the type of the keys of the values this package puts in a request context.
type contextKey int

const (
	clientKey contextKey = iota
	accessEntryKey
)

// forwardedFor is the header that carries the client address behind a reverse proxy.
const forwardedFor = "X-Forwarded-For"

// serverVersionHeader carries the version of splicefsd. Clients compare it with their own
// to refuse a server that is too old for them. It is spelled the way net/http writes
// header names (the docs say X-SpliceFS-Server-Version); header names are case-insensitive.
const serverVersionHeader = "X-Splicefs-Server-Version"

// reportVersion sets the version header before next runs, so that answers next writes
// itself and the 404, 405 and 500 answers around it all carry it.
func reportVersion(version string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(serverVersionHeader, version)
		next.ServeHTTP(w, r)
	})
}

// noSniff tells browsers to believe the Content-Type of every response.
func noSniff(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

// recoverPanics turns a panic in next into a logged 500. http.ErrAbortHandler is the
// standard way to abort a response and is passed on to the server untouched.
func recoverPanics(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// recover only works when the deferred function itself calls it, so the closure
		// reads the panic value and hands it on.
		defer func() { answerPanic(log, w, r, recover()) }()
		next.ServeHTTP(w, r)
	})
}

// answerPanic logs value, the value of a panic, and answers 500. A nil value means that
// there was no panic.
func answerPanic(log *slog.Logger, w http.ResponseWriter, r *http.Request, value any) {
	if value == nil {
		return
	}
	if err, ok := value.(error); ok && errors.Is(err, http.ErrAbortHandler) {
		panic(value)
	}
	log.ErrorContext(r.Context(), "panic in handler", slog.Any("panic", value),
		slog.String("method", r.Method), slog.String("path", r.URL.Path),
		slog.String("client", clientAddr(r.Context())),
		slog.String("stack", string(debug.Stack())))
	writeError(w, http.StatusInternalServerError, internalErrorMsg)
}

// accessEntry is what the access log learns while a request is handled.
type accessEntry struct {
	user string
}

// setAccessUser records the authenticated user for the access log of the request.
func setAccessUser(ctx context.Context, user string) {
	if entry, ok := ctx.Value(accessEntryKey).(*accessEntry); ok {
		entry.user = user
	}
}

// statusRecorder remembers the status and the size of a response.
type statusRecorder struct {
	http.ResponseWriter

	status int
	bytes  int64
}

// WriteHeader records the status and passes it on.
func (r *statusRecorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
	r.ResponseWriter.WriteHeader(status)
}

// Write counts the bytes written; a body sent without WriteHeader means status 200.
func (r *statusRecorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(p)
	r.bytes += int64(n)
	if err != nil {
		return n, fmt.Errorf("write response body: %w", err)
	}
	return n, nil
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

// logAccess logs one line per request at info level once next is done. It never logs a
// header or a body, so tokens and passwords stay out of the log.
func logAccess(log *slog.Logger, now func() time.Time, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := now()
		entry := &accessEntry{}
		rec := &statusRecorder{ResponseWriter: w}
		ctx := context.WithValue(r.Context(), accessEntryKey, entry)
		next.ServeHTTP(rec, r.WithContext(ctx))
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		attrs := []slog.Attr{
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", rec.status),
			slog.Int64("bytes", rec.bytes),
			slog.Duration("duration", now().Sub(start)),
			slog.String("client", clientAddr(r.Context())),
		}
		if entry.user != "" {
			attrs = append(attrs, slog.String("user", entry.user))
		}
		log.LogAttrs(r.Context(), slog.LevelInfo, "request", attrs...)
	})
}

// resolveClient puts the client address of the request in its context; see clientAddress.
func resolveClient(trusted []netip.Prefix, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), clientKey, clientAddress(r, trusted))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// clientAddr returns the client address that resolveClient stored in ctx.
func clientAddr(ctx context.Context) string {
	addr, ok := ctx.Value(clientKey).(string)
	if !ok {
		return ""
	}
	return addr
}

// clientAddress is the address of the connection, unless it comes from a trusted proxy.
// Then it is the rightmost X-Forwarded-For entry that is not itself a trusted proxy, so
// that entries a client wrote to the left of the proxies' own cannot be used to pick an
// address. A missing or unreadable header falls back to the address of the connection.
func clientAddress(r *http.Request, trusted []netip.Prefix) string {
	remote, ok := remoteIP(r.RemoteAddr)
	if !ok {
		return r.RemoteAddr
	}
	if !contains(trusted, remote) {
		return remote.String()
	}
	return forwardedClient(r.Header.Values(forwardedFor), trusted, remote).String()
}

// remoteIP extracts the IP address from the "host:port" of a connection.
func remoteIP(remoteAddr string) (netip.Addr, bool) {
	if addrPort, err := netip.ParseAddrPort(remoteAddr); err == nil {
		return addrPort.Addr().Unmap(), true
	}
	addr, err := netip.ParseAddr(remoteAddr)
	return addr.Unmap(), err == nil
}

// forwardedClient walks the X-Forwarded-For entries from the right and returns the first
// one that is not a trusted proxy, or remote when there is none or one cannot be read.
func forwardedClient(values []string, trusted []netip.Prefix, remote netip.Addr) netip.Addr {
	var entries []string
	for _, value := range values {
		for entry := range strings.SplitSeq(value, ",") {
			entries = append(entries, strings.TrimSpace(entry))
		}
	}
	for _, entry := range slices.Backward(entries) {
		addr, err := netip.ParseAddr(entry)
		if err != nil {
			return remote
		}
		if addr = addr.Unmap(); !contains(trusted, addr) {
			return addr
		}
	}
	return remote
}

// contains reports whether any prefix contains addr.
func contains(prefixes []netip.Prefix, addr netip.Addr) bool {
	for _, p := range prefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}
