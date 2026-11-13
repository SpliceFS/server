package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func listenLoopback(t *testing.T) *endpoint {
	t.Helper()
	listener, err := openEndpoint(context.Background(), "127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeQuietly(listener) })
	return listener
}

// newRunner is the serving loop of a run without what surrounds it: a server and its
// listeners, no generations and no reload.
func newRunner(handler http.Handler, opts Options) *runner {
	logger := slog.New(slog.DiscardHandler)
	return &runner{
		srv:       newServer(handler, logger),
		endpoints: make(map[string]*endpoint),
		failures:  make(chan error, 1),
		logger:    logger,
		opts:      opts,
	}
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
	r := newRunner(http.NotFoundHandler(), Options{Ready: func(addrs []net.Addr) { got = addrs }})
	broken := &endpoint{Listener: brokenListener{}}
	err := r.serve(context.Background(), []*endpoint{good, broken}, time.Second)
	if err == nil || !strings.Contains(err.Error(), "accept failed") ||
		!strings.Contains(err.Error(), "127.0.0.1:1") {
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
	r := newRunner(http.NotFoundHandler(), Options{})
	err := r.serve(context.Background(), []*endpoint{closed, other}, time.Second)
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
	r := newRunner(http.NotFoundHandler(), Options{Ready: func([]net.Addr) { close(ready) }})
	go func() { served <- r.serve(ctx, []*endpoint{retired, kept}, time.Second) }()
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
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client(nil).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer closeQuietly(resp.Body)
	return resp.StatusCode
}

func TestServeClosesConnectionsThatOutlastTheGrace(t *testing.T) {
	t.Parallel()
	listener := listenLoopback(t)
	started := make(chan struct{})
	handler := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	r := newRunner(handler, Options{})
	go func() { served <- r.serve(ctx, []*endpoint{listener}, 0) }()

	requestErr := make(chan error, 1)
	go func() {
		url := "http://" + listener.Addr().String() + "/"
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, http.NoBody)
		if err != nil {
			requestErr <- err
			return
		}
		resp, err := client(nil).Do(req)
		if err != nil {
			requestErr <- err
			return
		}
		defer closeQuietly(resp.Body)
		requestErr <- nil
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
	listener := listenLoopback(t)
	started, finish := make(chan struct{}), make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-finish
		respondText(w, "done")
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	r := newRunner(handler, Options{})
	go func() { served <- r.serve(ctx, []*endpoint{listener}, time.Minute) }()

	type result struct {
		body string
		err  error
	}
	results := make(chan result, 1)
	go func() {
		url := "http://" + listener.Addr().String() + "/"
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, http.NoBody)
		if err != nil {
			results <- result{err: err}
			return
		}
		resp, err := client(nil).Do(req)
		if err != nil {
			results <- result{err: err}
			return
		}
		defer closeQuietly(resp.Body)
		body, err := io.ReadAll(resp.Body)
		results <- result{body: string(body), err: err}
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
		t.Errorf("limits = %v %v %v %v %v", srv.ReadHeaderTimeout, srv.IdleTimeout,
			srv.MaxHeaderBytes, srv.WriteTimeout, srv.ReadTimeout)
	}
	srv.ErrorLog.Print("tls handshake error from 192.0.2.1")
	if out := logs.String(); !strings.Contains(out, "tls handshake error") || !strings.Contains(out, "level=WARN") {
		t.Errorf("ErrorLog output = %q", out)
	}
}
