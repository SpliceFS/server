package app

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"log/slog"
	"math/big"
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

	"github.com/SpliceFS/common/units"
	"github.com/SpliceFS/server/config"
)

// fixture is a complete server configuration over temporary files.
type fixture struct {
	cfg      *config.Config
	shareDir string
	usersDir string
	keysFile string
	nodes    string
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

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	f := &fixture{
		shareDir: filepath.Join(root, "media"),
		usersDir: filepath.Join(root, "users"),
	}
	hello := write(t, f.shareDir, "hello.bin", "0123456789")
	setMTime(t, hello, fileTime)
	page := write(t, f.shareDir, "page.html", htmlContent)
	setMTime(t, page, fileTime)
	write(t, f.shareDir, ".hidden", "never served")

	write(t, f.usersDir, "alice.yaml", "password_hash: \""+lowCostHash("right-password")+"\"\nshares: [media]\n")
	secret := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	f.keysFile = write(t, root, "keys.yaml", "keys:\n  - id: test\n    secret: \""+secret+"\"\n")
	f.nodes = write(t, root, "nodes.yaml", "cluster: example\nnodes:\n  - name: east\n    url: https://east.example.com/api\n")

	f.cfg = &config.Config{
		Listen:   []config.Listener{{Address: netip.MustParseAddr("127.0.0.1"), Port: 0}},
		BasePath: "/api",
		Shares: map[string]config.Share{
			"media": {Path: f.shareDir, Exclude: []string{".*"}},
		},
		Cluster: config.Cluster{Name: "example", Node: "east", NodesFile: f.nodes},
		Auth: config.Auth{
			UsersDir:  f.usersDir,
			KeysFile:  f.keysFile,
			TokenTTL:  units.Duration(30 * time.Minute),
			LoginRate: config.Rate{Count: 100, Per: time.Minute},
		},
		MTimePrecision: units.Duration(time.Second),
	}
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
func start(t *testing.T, cfg *config.Config) *running {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r := &running{cancel: cancel, done: make(chan error, 1), logs: new(bytes.Buffer)}
	ready := make(chan []net.Addr, 1)
	go func() {
		r.done <- Run(ctx, cfg, Options{
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

// url returns the URL of path on the first listener.
func (r *running) url(scheme, path string) string {
	return scheme + "://" + r.addrs[0].String() + path
}

// selfSigned writes a certificate and key for 127.0.0.1 and returns their paths and a
// pool that trusts the certificate.
func selfSigned(t *testing.T) (certFile, keyFile string, pool *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "splicefs test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile = write(t, dir, "cert.pem", string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})))
	keyFile = write(t, dir, "key.pem", string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})))
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool = x509.NewCertPool()
	pool.AddCert(cert)
	return certFile, keyFile, pool
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
