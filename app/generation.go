package app

import (
	"io"
	"log/slog"
	"net/http"
	"sync"
)

// generation is everything built from one configuration.
type generation struct {
	handler http.Handler
	closer  io.Closer      // what to close when the last request of this generation is done
	active  sync.WaitGroup // requests in progress
}

// live is the http.Handler of the server. It sends each request to the current generation, so a
// new configuration can be applied while requests are running: a request finishes against the
// configuration it started with, and new requests use the new one.
type live struct {
	mu       sync.RWMutex
	current  *generation
	retiring sync.WaitGroup // generations waiting for their last request
	logger   *slog.Logger
}

func newLive(first *generation, logger *slog.Logger) *live {
	return &live{current: first, logger: logger}
}

// ServeHTTP hands the request to the current generation.
func (l *live) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The generation is taken and counted under one read lock. replace swaps generations under
	// the write lock, so it cannot start waiting for a generation that a request has picked but
	// not yet counted, which would let the generation be closed while this request uses it.
	l.mu.RLock()
	g := l.current
	g.active.Add(1)
	l.mu.RUnlock()
	// A deferred Done also releases the count when the handler panics, so a panic cannot keep
	// the old generation open forever.
	defer g.active.Done()
	g.handler.ServeHTTP(w, r)
}

// replace makes next the generation for all new requests. The generation it replaces is closed
// in the background, after its last request has finished.
func (l *live) replace(next *generation) {
	l.mu.Lock()
	old := l.current
	l.current = next
	l.mu.Unlock()

	l.retiring.Add(1)
	go func() {
		defer l.retiring.Done()
		old.active.Wait()
		if err := old.closer.Close(); err != nil {
			l.logger.Warn("close replaced configuration", "error", err)
		}
	}()
}

// close is for shutdown. It must be called after the HTTP server has stopped, so no request
// and no replace can still arrive. It waits for every replaced generation and for the current
// one, closes the current one, and returns its close error.
func (l *live) close() error {
	l.retiring.Wait()
	l.mu.RLock()
	g := l.current
	l.mu.RUnlock()
	g.active.Wait()
	return g.closer.Close()
}
