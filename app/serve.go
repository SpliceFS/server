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

// listenAddress is the address an entry listens on, and the name a reload knows it by.
func listenAddress(entry config.Endpoint) string {
	return net.JoinHostPort(entry.Address.String(), strconv.Itoa(entry.Port))
}

// tlsConfigFor returns the TLS settings of a listener that is not plain: the configured
// certificate when the entry names files, and the certificate of the node otherwise. The node
// certificate is read for each connection, because it is made again while the server runs.
func tlsConfigFor(entry config.Endpoint, own *nodeCert) (*tls.Config, error) {
	if entry.TLS == nil {
		return &tls.Config{GetCertificate: own.get, MinVersion: tls.VersionTLS12}, nil
	}
	cert, err := tls.LoadX509KeyPair(entry.TLS.CertFile, entry.TLS.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("load certificate: %w", err)
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, nil
}

// closeAll closes every listener and reports all failures.
func closeAll(listeners []*endpoint) error {
	var errs []error
	for _, listener := range listeners {
		if err := listener.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// shutdown stops srv gracefully, and closes the remaining connections if the requests in
// progress do not finish within grace.
func shutdown(ctx context.Context, srv *http.Server, grace time.Duration) error {
	// The context of the caller is already cancelled; the wait has to outlive it.
	waitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), grace)
	defer cancel()
	if err := srv.Shutdown(waitCtx); err != nil {
		return wrap(srv.Close(), "close server")
	}
	return nil
}
