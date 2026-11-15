package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/SpliceFS/server/config"
	"github.com/SpliceFS/server/internal/porttest"
)

// fixture is a complete server: a configuration directory loaded the way the program
// loads it, and a state directory holding only what an administrator makes (the users).
// The server makes the rest on its first start.
type fixture struct {
	cfg      *config.Config
	state    string
	shareDir string
	data     dataPaths
}

// lowCostHash builds a valid argon2id PHC string with cheap parameters, so that a login
// in a test does not need 64 MiB.
func lowCostHash(password string) string {
	salt := []byte("0123456789abcdef")
	key := argon2.IDKey([]byte(password), salt, 1, 8, 1, 32)
	enc := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=19$m=8,t=1,p=1$%s$%s", enc.EncodeToString(salt), enc.EncodeToString(key))
}

func write(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// setMTime gives a file a fixed modification time, so its tag is known to the test.
func setMTime(t *testing.T, path string, mtime time.Time) {
	t.Helper()
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

// fileTime is the modification time that the files of a fixture get.
func fileTime() time.Time {
	return time.Date(2026, 10, 1, 8, 30, 0, 0, time.UTC)
}

// htmlContent is a compressible file of 2600 bytes.
func htmlContent() string {
	return strings.Repeat("<p>hello world</p>\n", 100)
}

// closeQuietly closes closer at the end of a test. The test has its result by then, and a
// close that fails because the other side closed first says nothing about it.
func closeQuietly(closer io.Closer) {
	_ = closer.Close() //nolint:errcheck // cleanup after the result is known
}

// respondText writes text as the body of a response. The client of a test handler is gone only
// when the test is over, so a failed write has nobody to be reported to.
func respondText(w io.Writer, text string) {
	_, _ = io.WriteString(w, text) //nolint:errcheck // see above
}

// loadConfig writes the files to a new configuration directory and loads it. A test needs
// a free port, which the validation refuses to take as 0, so the listener is replaced.
func loadConfig(t *testing.T, conf, shares map[string]string) *config.Config {
	t.Helper()
	dir := t.TempDir()
	for name, content := range conf {
		write(t, dir, filepath.Join("conf.d", name), content)
	}
	for name, content := range shares {
		write(t, dir, filepath.Join("shares.d", name), content)
	}
	cfg, err := config.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Listen = []config.Endpoint{configEndpoint(nil, true)}
	return cfg
}

// configEndpoint is a listener entry on the loopback address, with its port left unset. The
// server of a test is started through startWith, which gives every listener a port that
// porttest chose, and a test that runs the server by itself gives them with onFreePorts.
func configEndpoint(tlsFiles *config.TLS, plain bool) config.Endpoint {
	return config.Endpoint{
		Listener: config.Listener{Address: netip.MustParseAddr("127.0.0.1"), TLS: tlsFiles},
		Plain:    plain,
	}
}

// withPorts returns entries with the given ports, one for each. The entries it is given stay
// as they are.
func withPorts(entries []config.Endpoint, ports []int) []config.Endpoint {
	given := slices.Clone(entries)
	for i := range given {
		given[i].Port = ports[i]
	}
	return given
}

// onFreePorts gives the listeners of the fixture ports that porttest chose and calls attempt,
// which runs the server with them and returns why it did not start. When the failure is that
// a port is in use, the listeners get other ports and attempt runs again. The fixture keeps
// the ports of the attempt that succeeded.
func onFreePorts(t *testing.T, fix *fixture, attempt func() error) {
	t.Helper()
	porttest.TryMany(t, len(fix.cfg.Listen), func(ports []int) error {
		fix.cfg.Listen = withPorts(fix.cfg.Listen, ports)
		return attempt()
	})
}

// givePorts gives the listeners of the fixture ports for a Run that has to fail before it
// opens any, so that nothing can take them first.
func givePorts(t *testing.T, fix *fixture) {
	t.Helper()
	ports, err := porttest.PickMany(len(fix.cfg.Listen))
	if err != nil {
		t.Fatal(err)
	}
	fix.cfg.Listen = withPorts(fix.cfg.Listen, ports)
}

// plainOn is a plain listener entry on a loopback port. A test that gives a reload a second
// listener needs a port of its own, because a reload knows a listener by its configured
// address and port 0 on the loopback address is the one of the fixture.
func plainOn(port int) config.Endpoint {
	entry := configEndpoint(nil, true)
	entry.Port = port
	return entry
}

// failsAfterOpening calls attempt with a plain listener entry on a port that nothing holds.
// The attempt has to fail on purpose, after it opened that entry, and returns its failure. A
// failure of the entry itself because its port is in use means that another process took the
// port first, so the attempt is made again on another port. It returns the entry that was
// open when the attempt failed, and the failure.
func failsAfterOpening(t *testing.T, attempt func(opened config.Endpoint) error) (config.Endpoint, error) {
	t.Helper()
	var opened config.Endpoint
	var failure error
	porttest.Try(t, func(port int) error {
		opened = plainOn(port)
		failure = attempt(opened)
		if errors.Is(failure, syscall.EADDRINUSE) &&
			strings.Contains(failure.Error(), "listen on "+listenAddress(opened)+": ") {
			return failure
		}
		return nil
	})
	if failure == nil {
		t.Fatal("the attempt did not fail")
	}
	return opened, failure
}

// testSettings names the cluster, gives this node its public URL and allows many logins, so
// that tests do not wait for the rate limit.
const testSettings = "cluster:\n  name: example\n  node: east\n  url: https://east.example.com/api\n" +
	"auth:\n  login_rate: 100/m\n"

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	fix := &fixture{shareDir: filepath.Join(root, "media"), state: filepath.Join(root, "state")}
	fix.data = layout(fix.state)
	hello := write(t, fix.shareDir, "hello.bin", "0123456789")
	setMTime(t, hello, fileTime())
	page := write(t, fix.shareDir, "page.html", htmlContent())
	setMTime(t, page, fileTime())
	write(t, fix.shareDir, ".hidden", "never served")

	alice := "password_hash: \"" + lowCostHash("right-password") + "\"\nshares: [media]\n"
	write(t, fix.data.usersDir, "alice.yaml", alice)

	fix.cfg = loadConfig(t,
		map[string]string{"10-test.yaml": testSettings},
		map[string]string{"media.yaml": "path: " + fix.shareDir + "\n"})
	return fix
}

// syncBuffer is a log that the server writes while a test reads it.
type syncBuffer struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	changed chan struct{} // closed and replaced by every write, so a reader can wait for one
}

func newSyncBuffer() *syncBuffer {
	return &syncBuffer{changed: make(chan struct{})}
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	close(b.changed)
	b.changed = make(chan struct{})
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// waitFor returns when the log holds text at least count times, and fails the test if that
// takes longer than testTimeout.
func (b *syncBuffer) waitFor(t *testing.T, text string, count int) {
	t.Helper()
	timeout := time.After(testTimeout)
	for {
		b.mu.Lock()
		enough := strings.Count(b.buf.String(), text) >= count
		changed := b.changed
		b.mu.Unlock()
		if enough {
			return
		}
		select {
		case <-changed:
		case <-timeout:
			t.Fatalf("log does not hold %d times %q in time:\n%s", count, text, b)
		}
	}
}

// eventLog is a log handler that passes every record on to next and tells a test about two
// of them: the address of each "listening" record goes on listening, and the error of each
// "configuration not applied" record on refusals, in the order the server logged them. A
// value is sent after next has written the record, so a test that received it finds the line
// in the log. A test learns what the server did by receiving, never by waiting for time to
// pass. The channels hold eventBuffered values, so that the server never waits for the test.
type eventLog struct {
	next      slog.Handler
	listening chan string
	refusals  chan error
}

// eventBuffered is more of either record than any test causes.
const eventBuffered = 16

func newEventLog(next slog.Handler) *eventLog {
	return &eventLog{
		next:      next,
		listening: make(chan string, eventBuffered),
		refusals:  make(chan error, eventBuffered),
	}
}

// Enabled lets info records and above through even when next discards everything, because
// the records of a test are among them and a test that discards the log still needs them.
func (l *eventLog) Enabled(ctx context.Context, level slog.Level) bool {
	return level >= slog.LevelInfo || l.next.Enabled(ctx, level)
}

//nolint:gocritic // slog.Handler fixes the signature
func (l *eventLog) Handle(ctx context.Context, record slog.Record) error {
	err := l.next.Handle(ctx, record)
	switch record.Message {
	case "listening":
		l.listening <- attribute(record, "address").String()
	case "configuration not applied":
		l.refusals <- errorOf(attribute(record, "error"))
	default:
		// Every other record is only logged.
	}
	return err
}

func (l *eventLog) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &eventLog{next: l.next.WithAttrs(attrs), listening: l.listening, refusals: l.refusals}
}

func (l *eventLog) WithGroup(name string) slog.Handler {
	return &eventLog{next: l.next.WithGroup(name), listening: l.listening, refusals: l.refusals}
}

//nolint:gocritic // slog.Record is passed by value everywhere in slog
func attribute(record slog.Record, key string) slog.Value {
	var found slog.Value
	record.Attrs(func(attr slog.Attr) bool {
		if attr.Key == key {
			found = attr.Value
		}
		return true
	})
	return found
}

// errorOf returns the error a record carries as a value, which is what lets a test ask it
// what it wraps and not what it says.
func errorOf(value slog.Value) error {
	if err, ok := value.Any().(error); ok {
		return err
	}
	return errors.New(value.String())
}

// nextListening returns the address of the next "listening" record that the test did not
// receive yet. It waits only for the server to log it, and fails the test if that does not
// happen within testTimeout.
func (l *eventLog) nextListening(t *testing.T) string {
	t.Helper()
	select {
	case address := <-l.listening:
		return address
	case <-time.After(testTimeout):
		t.Fatal("the server did not log a listening address in time")
		return ""
	}
}

// assertNotListening fails the test if the server logged a listening address that the test
// did not receive. A test calls it after the step that could have logged one has returned,
// so a record that was ever going to come is already there.
func assertNotListening(t *testing.T, events *eventLog) {
	t.Helper()
	select {
	case address := <-events.listening:
		t.Errorf("the server logged that it listens on %s", address)
	default:
	}
}

// running is a server started with Run.
type running struct {
	addrs  []string // where the listeners of the configuration are, in its order
	events *eventLog
	cancel context.CancelFunc
	done   chan error
	logs   *syncBuffer
	once   sync.Once
	err    error
}

// start runs the server and waits until it is serving. The server is stopped when the test
// ends, if the test has not already stopped it.
func start(t *testing.T, fix *fixture) *running {
	t.Helper()
	return startWith(t, fix, nil)
}

// startWith is start, with the chance to set more options of Run.
func startWith(t *testing.T, fix *fixture, adjust func(*Options)) *running {
	t.Helper()
	var r *running
	onFreePorts(t, fix, func() error {
		var err error
		r, err = tryStart(t, fix, adjust)
		return err
	})
	return r
}

// tryStart makes one attempt to start the server with the ports the listeners have. It returns
// the error of a Run that ended before every listener was serving.
func tryStart(t *testing.T, fix *fixture, adjust func(*Options)) (*running, error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r := &running{cancel: cancel, done: make(chan error, 1), logs: newSyncBuffer()}
	r.events = newEventLog(slog.NewTextHandler(r.logs, nil))
	opts := Options{Version: "test", Logger: slog.New(r.events)}
	if adjust != nil {
		adjust(&opts)
	}
	go func() { r.done <- Run(ctx, fix.cfg, fix.state, opts) }()
	for range fix.cfg.Listen {
		select {
		case address := <-r.events.listening:
			r.addrs = append(r.addrs, address)
		case err := <-r.done:
			cancel()
			return nil, fmt.Errorf("run returned before every listener was serving: %w", err)
		case <-time.After(testTimeout):
			cancel()
			t.Fatalf("not every listener is serving in time:\n%s", r.logs)
		}
	}
	t.Cleanup(func() { r.stopForCleanup() })
	return r, nil
}

// stop cancels the server, waits for Run and returns what it returned. It can be called
// more than once.
func (r *running) stop() error {
	r.once.Do(func() {
		r.cancel()
		r.err = <-r.done
	})
	return r.err
}

// stopForCleanup stops the server at the end of a test. A test that cares what Run returned
// has stopped it already and checked, so the result is not checked again here.
func (r *running) stopForCleanup() {
	_ = r.stop() //nolint:errcheck // checked by the tests that care, see above
}

// url returns the plain HTTP URL of path on the first listener.
func (r *running) url(path string) string {
	return "http://" + r.addrs[0] + path
}

// certFiles is a certificate and key on disk, and a pool that trusts the certificate.
type certFiles struct {
	certFile, keyFile string
	pool              *x509.CertPool
}

// selfSigned makes a certificate and key for this machine and the loopback names with the
// code the server uses.
func selfSigned(t *testing.T) certFiles {
	t.Helper()
	dir := t.TempDir()
	if _, err := loadOrCreateCert(dir, testCertSource()); err != nil {
		t.Fatal(err)
	}
	certFile := filepath.Join(dir, certFileName)
	return certFiles{certFile: certFile, keyFile: filepath.Join(dir, keyFileName), pool: trusting(t, certFile)}
}

// testCertSource is the real clock and randomness with a fixed host name.
func testCertSource() certSource {
	return certSource{
		now:      time.Now,
		hostname: func() (string, error) { return "test-host", nil },
		random:   rand.Reader,
	}
}

// trusting returns a pool that trusts the certificate in the PEM file at path.
func trusting(t *testing.T, path string) *x509.CertPool {
	t.Helper()
	pool := x509.NewCertPool()
	data, err := os.ReadFile(path)
	if err != nil || !pool.AppendCertsFromPEM(data) {
		t.Fatalf("read certificate %s: %v", path, err)
	}
	return pool
}

// client returns an HTTP client that does not keep connections, so that shutdown never
// waits for an idle one, and that trusts pool if it is not nil.
func client(pool *x509.CertPool) *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DisableKeepAlives: true,
			TLSClientConfig:   tlsConfig(pool),
		},
	}
}

func tlsConfig(pool *x509.CertPool) *tls.Config {
	if pool == nil {
		return nil
	}
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
}

// response is a recorded response. Its body has been read by call.
type response struct {
	*http.Response

	body []byte
}

// isOpen reports whether something accepts connections at addr.
func isOpen(addr string) bool {
	dialer := net.Dialer{Timeout: time.Second}
	conn, err := dialer.DialContext(context.Background(), "tcp", addr)
	if err != nil {
		return false
	}
	closeQuietly(conn)
	return true
}
