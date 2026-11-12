package app

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/SpliceFS/server/config"
)

// Limits of the HTTP server. There is no write timeout, because a download of a large
// file legitimately takes as long as the client's connection needs.
const (
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 5 * time.Minute
	maxHeaderBytes    = 64 << 10
)

// newServer returns the http.Server for handler. Its internal errors go to logger.
func newServer(handler http.Handler, logger *slog.Logger) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
}

// certificateSource returns the certificate a listener without TLS files uses.
type certificateSource func() (tls.Certificate, error)

// listen opens a listener for every entry. If one fails, the ones opened before it are
// closed.
func listen(
	ctx context.Context, entries []config.Endpoint, own certificateSource,
) ([]net.Listener, error) {
	var listeners []net.Listener
	for _, entry := range entries {
		ln, err := listenOne(ctx, entry, own)
		if err != nil {
			return nil, errors.Join(err, closeAll(listeners))
		}
		listeners = append(listeners, ln)
	}
	return listeners, nil
}

// listenOne opens the listener of one entry, with TLS unless it asks for plain HTTP. The
// certificate is loaded before the port is bound, so a bad certificate does not leave a
// port open.
func listenOne(
	ctx context.Context, entry config.Endpoint, own certificateSource,
) (net.Listener, error) {
	address := net.JoinHostPort(entry.Address.String(), strconv.Itoa(entry.Port))
	tlsConfig, err := tlsConfigFor(entry, own)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", address, err)
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", address, err)
	}
	if tlsConfig != nil {
		ln = tls.NewListener(ln, tlsConfig)
	}
	return ln, nil
}

// tlsConfigFor returns nil for a plain listener, the configured certificate when the entry
// names files, and the server's own certificate otherwise.
func tlsConfigFor(entry config.Endpoint, own certificateSource) (*tls.Config, error) {
	if entry.Plain {
		return nil, nil
	}
	var cert tls.Certificate
	var err error
	if entry.TLS != nil {
		cert, err = tls.LoadX509KeyPair(entry.TLS.CertFile, entry.TLS.KeyFile)
	} else {
		cert, err = own()
	}
	if err != nil {
		return nil, fmt.Errorf("load certificate: %w", err)
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, nil
}

// closeAll closes every listener and reports all failures.
func closeAll(listeners []net.Listener) error {
	var errs []error
	for _, ln := range listeners {
		if err := ln.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// serve runs srv on every listener until ctx is cancelled or a listener fails, calling
// ready first. After cancellation it waits up to grace for requests in progress and then
// closes their connections; that is a clean shutdown and returns nil. After a failure it
// closes everything at once and returns the failure. It always waits for its goroutines.
func serve(ctx context.Context, srv *http.Server, listeners []net.Listener, grace time.Duration, ready func([]net.Addr)) error {
	if ready != nil {
		ready(addresses(listeners))
	}
	failures := make(chan error, len(listeners))
	var wg sync.WaitGroup
	for _, ln := range listeners {
		wg.Go(func() {
			if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
				failures <- fmt.Errorf("serve on %s: %w", ln.Addr(), err)
			}
		})
	}
	defer wg.Wait()

	select {
	case <-ctx.Done():
		return shutdown(ctx, srv, grace)
	case err := <-failures:
		return errors.Join(err, srv.Close())
	}
}

// shutdown stops srv gracefully, and closes the remaining connections if the requests in
// progress do not finish within grace.
func shutdown(ctx context.Context, srv *http.Server, grace time.Duration) error {
	// The context of the caller is already cancelled; the wait has to outlive it.
	waitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), grace)
	defer cancel()
	if err := srv.Shutdown(waitCtx); err != nil {
		return srv.Close()
	}
	return nil
}

// addresses returns the bound address of every listener.
func addresses(listeners []net.Listener) []net.Addr {
	addrs := make([]net.Addr, 0, len(listeners))
	for _, ln := range listeners {
		addrs = append(addrs, ln.Addr())
	}
	return addrs
}
