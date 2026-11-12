package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/SpliceFS/server/config"
)

// fixture is a complete server: a configuration directory loaded the way the program
// loads it, and a state directory holding the data the server reads.
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
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func remove(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}

// setMTime gives a file a fixed modification time, so its tag is known to the test.
func setMTime(t *testing.T, path string, mtime time.Time) {
	t.Helper()
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

var fileTime = time.Date(2026, 10, 1, 8, 30, 0, 0, time.UTC)

// htmlContent is a compressible file of 2600 bytes.
var htmlContent = strings.Repeat("<p>hello world</p>\n", 100)

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
	cfg.Listen = []config.Endpoint{endpoint(nil, true)}
	return cfg
}

// endpoint is a listener on a free loopback port.
func endpoint(tlsFiles *config.TLS, plain bool) config.Endpoint {
	return config.Endpoint{
		Listener: config.Listener{Address: netip.MustParseAddr("127.0.0.1"), TLS: tlsFiles},
		Plain:    plain,
	}
}

// testSettings names the cluster and allows many logins, so that tests do not wait for the
// rate limit.
const testSettings = "cluster:\n  name: example\n  node: east\nauth:\n  login_rate: 100/m\n"

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	f := &fixture{shareDir: filepath.Join(root, "media"), state: filepath.Join(root, "state")}
	f.data = layout(f.state)
	hello := write(t, f.shareDir, "hello.bin", "0123456789")
	setMTime(t, hello, fileTime)
	page := write(t, f.shareDir, "page.html", htmlContent)
	setMTime(t, page, fileTime)
	write(t, f.shareDir, ".hidden", "never served")

	alice := "password_hash: \"" + lowCostHash("right-password") + "\"\nshares: [media]\n"
	write(t, f.data.usersDir, "alice.yaml", alice)
	secret := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	write(t, filepath.Dir(f.data.keysFile), filepath.Base(f.data.keysFile),
		"keys:\n  - id: test\n    secret: \""+secret+"\"\n")
	write(t, filepath.Dir(f.data.nodesFile), filepath.Base(f.data.nodesFile),
		"cluster: ignored\nnodes:\n  - name: east\n    url: https://east.example.com/api\n")

	f.cfg = loadConfig(t,
		map[string]string{"10-test.yaml": testSettings},
		map[string]string{"media.yaml": "path: " + f.shareDir + "\n"})
	return f
}

// running is a server started with Run.
type running struct {
	addrs  []net.Addr
	cancel context.CancelFunc
	done   chan error
	logs   *bytes.Buffer
	once   sync.Once
	err    error
}

// start runs the server and waits until it is ready. The server is stopped when the test
// ends, if the test has not already stopped it.
func start(t *testing.T, f *fixture) *running {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r := &running{cancel: cancel, done: make(chan error, 1), logs: new(bytes.Buffer)}
	ready := make(chan []net.Addr, 1)
	go func() {
		r.done <- Run(ctx, f.cfg, f.state, Options{
			Version: "test",
			Logger:  slog.New(slog.NewTextHandler(r.logs, nil)),
			Ready:   func(addrs []net.Addr) { ready <- addrs },
		})
	}()
	select {
	case r.addrs = <-ready:
	case err := <-r.done:
		cancel()
		t.Fatalf("Run returned before it was ready: %v", err)
	}
	t.Cleanup(func() { _ = r.stop() })
	return r
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

// url returns the plain HTTP URL of path on the first listener.
func (r *running) url(path string) string {
	return "http://" + r.addrs[0].String() + path
}

// selfSigned makes a certificate and key for this machine and the loopback names with the
// code the server uses, and returns their paths and a pool that trusts the certificate.
func selfSigned(t *testing.T) (certFile, keyFile string, pool *x509.CertPool) {
	t.Helper()
	dir := t.TempDir()
	if _, err := loadOrCreateCert(dir, testCertSource()); err != nil {
		t.Fatal(err)
	}
	certFile, keyFile = filepath.Join(dir, certFileName), filepath.Join(dir, keyFileName)
	return certFile, keyFile, trusting(t, certFile)
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
type response struct{ *http.Response }

// isOpen reports whether something accepts connections at addr.
func isOpen(addr string) bool {
	dialer := net.Dialer{Timeout: time.Second}
	conn, err := dialer.DialContext(context.Background(), "tcp", addr)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
