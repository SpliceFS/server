package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// testTimeout bounds every connection a test makes.
const testTimeout = 5 * time.Second

// loopbackCert is a certificate for the loopback addresses, in the form a server loads and
// in the form a client trusts.
type loopbackCert struct {
	pair *tls.Certificate
	leaf *x509.Certificate
}

// testCert makes a certificate for the loopback addresses.
func testCert(t *testing.T) loopbackCert {
	t.Helper()
	files := selfSigned(t)
	cert, err := tls.LoadX509KeyPair(files.certFile, files.keyFile)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return loopbackCert{pair: &cert, leaf: leaf}
}

func serverConfig(cert *tls.Certificate) *tls.Config {
	return &tls.Config{Certificates: []tls.Certificate{*cert}, MinVersion: tls.VersionTLS12}
}

func poolOf(certs ...*x509.Certificate) *x509.CertPool {
	pool := x509.NewCertPool()
	for _, cert := range certs {
		pool.AddCert(cert)
	}
	return pool
}

func openLoopback(t *testing.T, cfg *tls.Config) *endpoint {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	listener, err := openEndpoint(ctx, "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeQuietly(listener) })
	return listener
}

// serveOnce accepts one connection of listener in the background and writes "ok" to it. The
// channel receives what went wrong on the server side, or nil.
func serveOnce(listener *endpoint) <-chan error {
	served := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			served <- err
			return
		}
		defer closeQuietly(conn)
		if err = conn.SetDeadline(time.Now().Add(testTimeout)); err != nil {
			served <- err
			return
		}
		_, err = conn.Write([]byte("ok"))
		served <- err
	}()
	return served
}

// finishRoundTrip reads what serveOnce writes and fails the test if either side went wrong.
func finishRoundTrip(t *testing.T, conn net.Conn, served <-chan error) {
	t.Helper()
	defer closeQuietly(conn)
	if err := conn.SetDeadline(time.Now().Add(testTimeout)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 2)
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "ok" {
		t.Fatalf("read %q: %v", buf, err)
	}
	if err := <-served; err != nil {
		t.Fatalf("server side: %v", err)
	}
}

// plainRoundTrip makes one connection to listener without TLS and reads what the server side
// writes.
func plainRoundTrip(t *testing.T, listener *endpoint) error {
	t.Helper()
	served := serveOnce(listener)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", listener.Addr().String())
	if err != nil {
		<-served //nolint:errcheck // the dial error is the one to report
		return err
	}
	finishRoundTrip(t, conn, served)
	return nil
}

// tlsRoundTrip makes one TLS connection to listener, with a client that trusts pool, and
// reads what the server side writes. It returns the certificate the client was shown.
func tlsRoundTrip(t *testing.T, listener *endpoint, pool *x509.CertPool) (*x509.Certificate, error) {
	t.Helper()
	served := serveOnce(listener)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	dialer := tls.Dialer{Config: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
	conn, err := dialer.DialContext(ctx, "tcp", listener.Addr().String())
	if err != nil {
		<-served //nolint:errcheck // the dial error is the one to report
		return nil, err
	}
	tlsConn, ok := conn.(*tls.Conn)
	if !ok {
		t.Fatalf("a TLS dialer returned a %T", conn)
	}
	seen := tlsConn.ConnectionState().PeerCertificates[0]
	finishRoundTrip(t, conn, served)
	return seen, nil
}

// transport is how an endpoint serves its connections.
type transport int

const (
	plainHTTP transport = iota
	tlsConnection
)

// serving returns the TLS settings that make an endpoint serve kind.
func (c loopbackCert) serving(kind transport) *tls.Config {
	if kind == tlsConnection {
		return serverConfig(c.pair)
	}
	return nil
}

// expectTransport fails the test unless one connection to listener is served as kind.
func expectTransport(t *testing.T, listener *endpoint, cert loopbackCert, kind transport, step int) {
	t.Helper()
	if kind == tlsConnection {
		seen, err := tlsRoundTrip(t, listener, poolOf(cert.leaf))
		if err != nil || !seen.Equal(cert.leaf) {
			t.Errorf("step %d (TLS): certificate %v, error %v", step, seen, err)
		}
		return
	}
	if err := plainRoundTrip(t, listener); err != nil {
		t.Errorf("step %d (plain): error %v", step, err)
	}
}

func TestEndpointAcceptsPlainHTTP(t *testing.T) {
	t.Parallel()
	listener := openLoopback(t, nil)
	if err := plainRoundTrip(t, listener); err != nil {
		t.Errorf("plain connection: error %v", err)
	}
}

func TestEndpointAcceptsTLS(t *testing.T) {
	t.Parallel()
	cert := testCert(t)
	listener := openLoopback(t, serverConfig(cert.pair))
	seen, err := tlsRoundTrip(t, listener, poolOf(cert.leaf))
	if err != nil || !seen.Equal(cert.leaf) {
		t.Errorf("TLS connection: certificate %v, error %v", seen, err)
	}
}

func TestEndpointSetTLSChangesTheCertificateOnTheSamePort(t *testing.T) {
	t.Parallel()
	first, second := testCert(t), testCert(t)
	both := poolOf(first.leaf, second.leaf)
	listener := openLoopback(t, serverConfig(first.pair))
	addr := listener.Addr().String()

	if seen, err := tlsRoundTrip(t, listener, both); err != nil || !seen.Equal(first.leaf) {
		t.Fatalf("before: certificate %v, error %v", seen, err)
	}
	listener.setTLS(serverConfig(second.pair))
	if seen, err := tlsRoundTrip(t, listener, both); err != nil || !seen.Equal(second.leaf) {
		t.Errorf("after: certificate %v, error %v", seen, err)
	}
	if listener.Addr().String() != addr {
		t.Errorf("the address changed from %s to %s", addr, listener.Addr())
	}
	if _, err := tlsRoundTrip(t, listener, poolOf(first.leaf)); err == nil {
		t.Error("the old certificate is still trusted by the new connection")
	}
}

func TestEndpointSwitchesBetweenTLSAndPlain(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		steps []transport
	}{
		{"TLS to plain and back", []transport{tlsConnection, plainHTTP, tlsConnection}},
		{"plain to TLS and back", []transport{plainHTTP, tlsConnection, plainHTTP}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cert := testCert(t)
			listener := openLoopback(t, cert.serving(tt.steps[0]))
			addr := listener.Addr().String()
			for step, kind := range tt.steps {
				listener.setTLS(cert.serving(kind))
				expectTransport(t, listener, cert, kind, step)
				if listener.Addr().String() != addr {
					t.Errorf("step %d: the address changed to %s", step, listener.Addr())
				}
			}
		})
	}
}

func TestEndpointRetireClosesThePort(t *testing.T) {
	t.Parallel()
	listener := openLoopback(t, nil)
	if listener.isRetired() {
		t.Fatal("a new endpoint is retired")
	}
	if err := listener.retire(); err != nil {
		t.Fatalf("retire = %v", err)
	}
	if !listener.isRetired() {
		t.Error("isRetired is false after retire")
	}
	if conn, err := listener.Accept(); err == nil {
		closeQuietly(conn)
		t.Error("Accept succeeded on a retired endpoint")
	}
	if isOpen(listener.Addr().String()) {
		t.Error("the port is still open")
	}
}

func TestEndpointRetireReportsAFailureToClose(t *testing.T) {
	t.Parallel()
	listener := &endpoint{Listener: brokenListener{}}
	if err := listener.retire(); err == nil || !strings.Contains(err.Error(), "close failed") {
		t.Errorf("retire = %v, want the close failure", err)
	}
	if !listener.isRetired() {
		t.Error("a listener whose close failed is not marked as retired")
	}
}

func TestOpenEndpointFailsWhenTheAddressIsInUse(t *testing.T) {
	t.Parallel()
	taken := openLoopback(t, nil)
	listener, err := openEndpoint(context.Background(), taken.Addr().String(), nil)
	if err == nil {
		closeQuietly(listener)
		t.Fatal("openEndpoint succeeded on an address in use")
	}
	if listener != nil {
		t.Errorf("openEndpoint returned %v with its error", listener)
	}
}
