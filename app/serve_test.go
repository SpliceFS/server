package app

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func listenLoopback(t *testing.T) *endpoint {
	t.Helper()
	ln, err := openEndpoint(context.Background(), "127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

// brokenListener is a listener that cannot accept and cannot be closed cleanly.
type brokenListener struct {
	net.Listener
}

func (brokenListener) Accept() (net.Conn, error) { return nil, errors.New("accept failed") }
func (brokenListener) Close() error              { return errors.New("close failed") }
func (brokenListener) Addr() net.Addr            { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1} }

func TestServeStopsEverythingWhenAListenerFails(t *testing.T) {
	t.Parallel()
	good := listenLoopback(t)
	var got []net.Addr
	srv := newServer(http.NotFoundHandler(), slog.New(slog.DiscardHandler))
	broken := &endpoint{Listener: brokenListener{}}
	err := serve(context.Background(), srv, []*endpoint{good, broken}, time.Second,
		func(addrs []net.Addr) { got = addrs })
	if err == nil || !strings.Contains(err.Error(), "accept failed") || !strings.Contains(err.Error(), "127.0.0.1:1") {
		t.Errorf("serve = %v", err)
	}
	if len(got) != 2 || got[0].String() != good.Addr().String() {
		t.Errorf("Ready got %v", got)
	}
	if isOpen(good.Addr().String()) {
		t.Error("the healthy listener is still open")
	}
}

func TestServeTreatsAnEndpointClosedWithoutRetireAsAFailure(t *testing.T) {
	t.Parallel()
	closed, other := listenLoopback(t), listenLoopback(t)
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	srv := newServer(http.NotFoundHandler(), slog.New(slog.DiscardHandler))
	err := serve(context.Background(), srv, []*endpoint{closed, other}, time.Second, nil)
	if err == nil || !strings.Contains(err.Error(), "serve on "+closed.Addr().String()) {
		t.Errorf("serve = %v", err)
	}
	if closed.isRetired() {
		t.Error("the endpoint was never retired")
	}
	if isOpen(other.Addr().String()) {
		t.Error("the other endpoint is still open")
	}
}

func TestServeIgnoresARetiredEndpointAndKeepsTheOthers(t *testing.T) {
	t.Parallel()
	retired, kept := listenLoopback(t), listenLoopback(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ready := make(chan struct{})
	served := make(chan error, 1)
	go func() {
		served <- serve(ctx, newServer(http.NotFoundHandler(), slog.New(slog.DiscardHandler)),
			[]*endpoint{retired, kept}, time.Second, func([]net.Addr) { close(ready) })
	}()
	<-ready

	if err := retired.retire(); err != nil {
		t.Fatal(err)
	}
	waitClosed(t, retired.Addr().String())
	if status := statusOf(t, "http://"+kept.Addr().String()+"/"); status != http.StatusNotFound {
		t.Errorf("the endpoint that stayed answers %d, want 404", status)
	}
	select {
	case err := <-served:
		t.Fatalf("serve returned %v after one endpoint was retired", err)
	default:
	}
	cancel()
	if err := <-served; err != nil {
		t.Errorf("serve = %v, want nil on cancel", err)
	}
}

// waitClosed fails the test if nothing stops accepting at addr within a few seconds.
func waitClosed(t *testing.T, addr string) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); isOpen(addr); {
		if time.Now().After(deadline) {
			t.Fatalf("%s still accepts connections", addr)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// statusOf returns the status code of a GET of url.
func statusOf(t *testing.T, url string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client(nil).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func TestServeClosesConnectionsThatOutlastTheGrace(t *testing.T) {
	t.Parallel()
	ln := listenLoopback(t)
	started := make(chan struct{})
	handler := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() {
		served <- serve(ctx, newServer(handler, slog.New(slog.DiscardHandler)), []*endpoint{ln}, 0, nil)
	}()

	requestErr := make(chan error, 1)
	go func() {
		req, err := http.NewRequestWithContext(context.Background(), "GET", "http://"+ln.Addr().String()+"/", nil)
		if err != nil {
			requestErr <- err
			return
		}
		resp, err := client(nil).Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
		requestErr <- err
	}()
	<-started
	cancel()
	if err := <-served; err != nil {
		t.Errorf("serve = %v, want nil after a forced shutdown", err)
	}
	if err := <-requestErr; err == nil {
		t.Error("the request that was in progress was not cut off")
	}
}

func TestServeWaitsForRequestsWithinTheGrace(t *testing.T) {
	t.Parallel()
	ln := listenLoopback(t)
	started, finish := make(chan struct{}), make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-finish
		_, _ = w.Write([]byte("done"))
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() {
		srv := newServer(handler, slog.New(slog.DiscardHandler))
		served <- serve(ctx, srv, []*endpoint{ln}, time.Minute, nil)
	}()

	type result struct {
		body string
		err  error
	}
	results := make(chan result, 1)
	go func() {
		req, err := http.NewRequestWithContext(context.Background(), "GET", "http://"+ln.Addr().String()+"/", nil)
		if err != nil {
			results <- result{err: err}
			return
		}
		resp, err := client(nil).Do(req)
		if err != nil {
			results <- result{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		buf := make([]byte, 16)
		n, _ := resp.Body.Read(buf)
		results <- result{body: string(buf[:n])}
	}()
	<-started
	cancel()
	// Shutdown has stopped accepting by now or will in a moment; either way the
	// handler is still allowed to finish.
	close(finish)
	if r := <-results; r.err != nil || r.body != "done" {
		t.Errorf("request: %q, %v", r.body, r.err)
	}
	if err := <-served; err != nil {
		t.Errorf("serve = %v", err)
	}
}

func TestCloseAllReportsEveryFailure(t *testing.T) {
	t.Parallel()
	good := listenLoopback(t)
	err := closeAll([]*endpoint{{Listener: brokenListener{}}, good, {Listener: brokenListener{}}})
	if err == nil || strings.Count(err.Error(), "close failed") != 2 {
		t.Errorf("closeAll = %v", err)
	}
	if isOpen(good.Addr().String()) {
		t.Error("the healthy listener was not closed")
	}
}

func TestNewServerLimits(t *testing.T) {
	t.Parallel()
	var logs strings.Builder
	srv := newServer(http.NotFoundHandler(), slog.New(slog.NewTextHandler(&logs, nil)))
	if srv.ReadHeaderTimeout != 10*time.Second || srv.IdleTimeout != 5*time.Minute ||
		srv.MaxHeaderBytes != 64<<10 || srv.WriteTimeout != 0 || srv.ReadTimeout != 0 {
		t.Errorf("limits = %v %v %v %v %v", srv.ReadHeaderTimeout, srv.IdleTimeout, srv.MaxHeaderBytes, srv.WriteTimeout, srv.ReadTimeout)
	}
	srv.ErrorLog.Print("tls handshake error from 192.0.2.1")
	if out := logs.String(); !strings.Contains(out, "tls handshake error") || !strings.Contains(out, "level=WARN") {
		t.Errorf("ErrorLog output = %q", out)
	}
}
