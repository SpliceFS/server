package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"testing"
	"time"
)

// testTimeout bounds every connection a test makes.
const testTimeout = 5 * time.Second

// testCert makes a certificate for the loopback addresses and returns it with its parsed
// form.
func testCert(t *testing.T) (tls.Certificate, *x509.Certificate) {
	t.Helper()
	certFile, keyFile, _ := selfSigned(t)
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return cert, leaf
}

func serverConfig(cert tls.Certificate) *tls.Config {
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
}

func poolOf(certs ...*x509.Certificate) *x509.CertPool {
	pool := x509.NewCertPool()
	for _, c := range certs {
		pool.AddCert(c)
	}
	return pool
}

func openLoopback(t *testing.T, cfg *tls.Config) *endpoint {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	e, err := openEndpoint(ctx, "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	return e
}

// roundTrip makes one connection to e and reads what the server side writes. A nil pool
// connects without TLS. For TLS it returns the certificate the client was shown.
func roundTrip(t *testing.T, e *endpoint, pool *x509.CertPool) (*x509.Certificate, error) {
	t.Helper()
	served := make(chan error, 1)
	go func() {
		conn, err := e.Accept()
		if err != nil {
			served <- err
			return
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetDeadline(time.Now().Add(testTimeout))
		_, err = conn.Write([]byte("ok"))
		served <- err
	}()

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	var conn net.Conn
	var err error
	if pool == nil {
		var d net.Dialer
		conn, err = d.DialContext(ctx, "tcp", e.Addr().String())
	} else {
		d := tls.Dialer{Config: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
		conn, err = d.DialContext(ctx, "tcp", e.Addr().String())
	}
	if err != nil {
		<-served
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(testTimeout))
	buf := make([]byte, 2)
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "ok" {
		t.Fatalf("read %q: %v", buf, err)
	}
	if err := <-served; err != nil {
		t.Fatalf("server side: %v", err)
	}
	if tlsConn, ok := conn.(*tls.Conn); ok {
		return tlsConn.ConnectionState().PeerCertificates[0], nil
	}
	return nil, nil
}

func TestEndpointAcceptsPlainHTTP(t *testing.T) {
	t.Parallel()
	e := openLoopback(t, nil)
	if seen, err := roundTrip(t, e, nil); err != nil || seen != nil {
		t.Errorf("plain connection: certificate %v, error %v", seen, err)
	}
}

func TestEndpointAcceptsTLS(t *testing.T) {
	t.Parallel()
	cert, leaf := testCert(t)
	e := openLoopback(t, serverConfig(cert))
	seen, err := roundTrip(t, e, poolOf(leaf))
	if err != nil || !seen.Equal(leaf) {
		t.Errorf("TLS connection: certificate %v, error %v", seen, err)
	}
}

func TestEndpointSetTLSChangesTheCertificateOnTheSamePort(t *testing.T) {
	t.Parallel()
	first, firstLeaf := testCert(t)
	second, secondLeaf := testCert(t)
	both := poolOf(firstLeaf, secondLeaf)
	e := openLoopback(t, serverConfig(first))
	addr := e.Addr().String()

	if seen, err := roundTrip(t, e, both); err != nil || !seen.Equal(firstLeaf) {
		t.Fatalf("before: certificate %v, error %v", seen, err)
	}
	e.setTLS(serverConfig(second))
	if seen, err := roundTrip(t, e, both); err != nil || !seen.Equal(secondLeaf) {
		t.Errorf("after: certificate %v, error %v", seen, err)
	}
	if e.Addr().String() != addr {
		t.Errorf("the address changed from %s to %s", addr, e.Addr())
	}
	if _, err := roundTrip(t, e, poolOf(firstLeaf)); err == nil {
		t.Error("the old certificate is still trusted by the new connection")
	}
}

func TestEndpointSwitchesBetweenTLSAndPlain(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		steps []bool // true serves TLS, false serves plain HTTP
	}{
		{"TLS to plain and back", []bool{true, false, true}},
		{"plain to TLS and back", []bool{false, true, false}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cert, leaf := testCert(t)
			cfg := func(useTLS bool) *tls.Config {
				if useTLS {
					return serverConfig(cert)
				}
				return nil
			}
			e := openLoopback(t, cfg(tt.steps[0]))
			addr := e.Addr().String()
			for i, useTLS := range tt.steps {
				e.setTLS(cfg(useTLS))
				var pool *x509.CertPool
				if useTLS {
					pool = poolOf(leaf)
				}
				seen, err := roundTrip(t, e, pool)
				if err != nil || (seen != nil) != useTLS {
					t.Errorf("step %d (TLS %v): certificate %v, error %v", i, useTLS, seen, err)
				}
				if e.Addr().String() != addr {
					t.Errorf("step %d: the address changed to %s", i, e.Addr())
				}
			}
		})
	}
}

func TestEndpointRetireClosesThePort(t *testing.T) {
	t.Parallel()
	e := openLoopback(t, nil)
	if e.isRetired() {
		t.Fatal("a new endpoint is retired")
	}
	if err := e.retire(); err != nil {
		t.Fatalf("retire = %v", err)
	}
	if !e.isRetired() {
		t.Error("isRetired is false after retire")
	}
	if conn, err := e.Accept(); err == nil {
		_ = conn.Close()
		t.Error("Accept succeeded on a retired endpoint")
	}
	if isOpen(e.Addr().String()) {
		t.Error("the port is still open")
	}
}

func TestOpenEndpointFailsWhenTheAddressIsInUse(t *testing.T) {
	t.Parallel()
	taken := openLoopback(t, nil)
	e, err := openEndpoint(context.Background(), taken.Addr().String(), nil)
	if err == nil {
		_ = e.Close()
		t.Fatal("openEndpoint succeeded on an address in use")
	}
	if e != nil {
		t.Errorf("openEndpoint returned %v with its error", e)
	}
}
