package app

import (
	"context"
	"crypto/tls"
	"net"
	"sync/atomic"
)

// endpoint is one listening address. It keeps the TCP listener open for as long as the
// address stays configured, so that a reload can change the certificate or switch between
// TLS and plain HTTP without closing the port.
type endpoint struct {
	net.Listener                            // the TCP listener
	address      string                     // as configured; what a reload compares
	tlsConfig    atomic.Pointer[tls.Config] // nil serves plain HTTP
	retired      atomic.Bool
}

// openEndpoint binds address and serves TLS with tlsConfig, or plain HTTP when it is nil.
func openEndpoint(ctx context.Context, address string, tlsConfig *tls.Config) (*endpoint, error) {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}
	e := &endpoint{Listener: ln, address: address}
	e.tlsConfig.Store(tlsConfig)
	return e, nil
}

// Accept returns the next connection, wrapped in TLS if a configuration is set. The
// configuration is read per connection, so a change applies to the next one.
func (e *endpoint) Accept() (net.Conn, error) {
	conn, err := e.Listener.Accept()
	if err != nil {
		return nil, err
	}
	if cfg := e.tlsConfig.Load(); cfg != nil {
		return tls.Server(conn, cfg), nil
	}
	return conn, nil
}

// setTLS replaces the TLS configuration for new connections. Nil switches to plain HTTP.
// Connections that are already open keep what they negotiated.
func (e *endpoint) setTLS(cfg *tls.Config) {
	e.tlsConfig.Store(cfg)
}

// retire closes the listener on purpose. The mark is set first, so that the server loop
// that sees Accept fail can tell this from a failure.
func (e *endpoint) retire() error {
	e.retired.Store(true)
	return e.Close()
}

// isRetired reports whether retire has been called.
func (e *endpoint) isRetired() bool {
	return e.retired.Load()
}
