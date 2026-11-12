package app

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// genWait bounds every wait in these tests, so a bug fails the test instead of hanging it.
const genWait = 5 * time.Second

// genCloser records how often it was closed and what the handler had done by then.
type genCloser struct {
	err    error
	mu     sync.Mutex
	count  int
	closed chan struct{} // closed on the first Close
	// finished, when set, is read at Close time to learn whether the request had finished.
	finished      *atomic.Bool
	finishedAtEnd bool
}

func newGenCloser(err error) *genCloser {
	return &genCloser{err: err, closed: make(chan struct{})}
}

func (c *genCloser) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.count++
	if c.count == 1 {
		if c.finished != nil {
			c.finishedAtEnd = c.finished.Load()
		}
		close(c.closed)
	}
	return c.err
}

func (c *genCloser) calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.count
}

// isClosed reports without waiting whether Close was called.
func (c *genCloser) isClosed() bool {
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}

func (c *genCloser) waitClosed(t *testing.T) {
	t.Helper()
	select {
	case <-c.closed:
	case <-time.After(genWait):
		t.Fatal("generation was not closed in time")
	}
}

// genNamed answers every request with its name, so a test can see which generation ran.
func genNamed(name string, closer *genCloser) *generation {
	return &generation{
		handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(name))
		}),
		closer: closer,
	}
}

func genGet(l *live) string {
	rec := httptest.NewRecorder()
	l.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil))
	return rec.Body.String()
}

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func TestLiveRequestReachesCurrentHandler(t *testing.T) {
	t.Parallel()
	l := newLive(genNamed("first", newGenCloser(nil)), discardLogger())
	if got := genGet(l); got != "first" {
		t.Fatalf("got %q, want first", got)
	}
}

func TestLiveReplaceSendsNewRequestsToNewHandler(t *testing.T) {
	t.Parallel()
	oldCloser := newGenCloser(nil)
	l := newLive(genNamed("old", oldCloser), discardLogger())
	l.replace(genNamed("new", newGenCloser(nil)))
	if got := genGet(l); got != "new" {
		t.Fatalf("got %q, want new", got)
	}
	oldCloser.waitClosed(t)
}

func TestLiveRequestInProgressFinishesOnOldGeneration(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	release := make(chan struct{})
	var finished atomic.Bool
	oldCloser := newGenCloser(nil)
	oldCloser.finished = &finished
	old := &generation{
		handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			close(started)
			<-release
			_, _ = w.Write([]byte("old"))
			finished.Store(true)
		}),
		closer: oldCloser,
	}
	l := newLive(old, discardLogger())

	done := make(chan string)
	go func() { done <- genGet(l) }()
	select {
	case <-started:
	case <-time.After(genWait):
		t.Fatal("request did not start")
	}

	l.replace(genNamed("new", newGenCloser(nil)))
	if got := genGet(l); got != "new" {
		t.Fatalf("new request got %q, want new", got)
	}
	// The wait here cannot decide the result: a wrong implementation that closes early has
	// already done so by the time the new request returned, or does so within this wait.
	time.Sleep(5 * time.Millisecond)
	if oldCloser.isClosed() {
		t.Fatal("old generation closed while its request was still running")
	}

	close(release)
	select {
	case got := <-done:
		if got != "old" {
			t.Fatalf("request in progress got %q, want old", got)
		}
	case <-time.After(genWait):
		t.Fatal("request in progress did not finish")
	}
	oldCloser.waitClosed(t)
	if !oldCloser.finishedAtEnd {
		t.Fatal("old generation closed before its request finished")
	}
}

func TestLiveSeveralReplacesCloseEveryOldGenerationOnce(t *testing.T) {
	t.Parallel()
	closers := []*genCloser{newGenCloser(nil), newGenCloser(nil), newGenCloser(nil)}
	l := newLive(genNamed("g0", closers[0]), discardLogger())
	l.replace(genNamed("g1", closers[1]))
	l.replace(genNamed("g2", closers[2]))
	l.replace(genNamed("g3", newGenCloser(nil)))
	l.retiring.Wait()
	for i, c := range closers {
		if got := c.calls(); got != 1 {
			t.Errorf("generation %d closed %d times, want 1", i, got)
		}
	}
	if got := genGet(l); got != "g3" {
		t.Fatalf("got %q, want g3", got)
	}
}

func TestLiveCloseErrorOfReplacedGenerationIsLogged(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	l := newLive(genNamed("old", newGenCloser(errors.New("disk gone"))),
		slog.New(slog.NewTextHandler(&logs, nil)))
	l.replace(genNamed("new", newGenCloser(nil)))
	// Waiting for the retiring generation also orders its log write before this read.
	l.retiring.Wait()
	out := logs.String()
	for _, want := range []string{"level=WARN", "close replaced configuration", "disk gone"} {
		if !strings.Contains(out, want) {
			t.Errorf("log %q does not contain %q", out, want)
		}
	}
}

func TestLiveCloseReturnsCurrentCloseError(t *testing.T) {
	t.Parallel()
	want := errors.New("close failed")
	oldCloser := newGenCloser(nil)
	l := newLive(genNamed("old", oldCloser), discardLogger())
	l.replace(genNamed("new", newGenCloser(want)))
	if err := l.close(); !errors.Is(err, want) {
		t.Fatalf("close returned %v, want %v", err, want)
	}
	if got := oldCloser.calls(); got != 1 {
		t.Fatalf("replaced generation closed %d times, want 1", got)
	}
}

func TestLiveCloseWaitsForRequestsInProgress(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	release := make(chan struct{})
	closer := newGenCloser(nil)
	l := newLive(&generation{
		handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			close(started)
			<-release
		}),
		closer: closer,
	}, discardLogger())

	go genGet(l)
	select {
	case <-started:
	case <-time.After(genWait):
		t.Fatal("request did not start")
	}
	result := make(chan error)
	go func() { result <- l.close() }()
	time.Sleep(5 * time.Millisecond)
	if closer.isClosed() {
		t.Fatal("current generation closed while its request was still running")
	}
	close(release)
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("close returned %v", err)
		}
	case <-time.After(genWait):
		t.Fatal("close did not return")
	}
	if !closer.isClosed() {
		t.Fatal("current generation not closed")
	}
}

func TestGenerationPanicReleasesRequestCount(t *testing.T) {
	t.Parallel()
	closer := newGenCloser(nil)
	l := newLive(&generation{
		handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("handler broke") }),
		closer:  closer,
	}, discardLogger())

	func() {
		defer func() {
			if got := recover(); got != "handler broke" {
				t.Errorf("recovered %v, want the handler panic", got)
			}
		}()
		genGet(l)
	}()

	l.replace(genNamed("new", newGenCloser(nil)))
	closer.waitClosed(t)
}
