package app

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/SpliceFS/common/wire"
	"github.com/SpliceFS/server/config"
	"github.com/SpliceFS/server/internal/datadir"
	"github.com/SpliceFS/server/share"
)

// call sends a request and returns the response with its whole body read.
func call(
	t *testing.T, httpClient *http.Client, method, url, token string, header map[string]string, body string,
) response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for key, value := range header {
		req.Header.Set(key, value)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer closeQuietly(resp.Body)
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response{Response: resp, body: data}
}

func decode(t *testing.T, data []byte, target any) {
	t.Helper()
	if err := json.Unmarshal(data, target); err != nil {
		t.Fatalf("%v in %q", err, data)
	}
}

// goodLogin is the login of the user that newFixture makes.
const goodLogin = `{"user":"alice","password":"right-password"}`

// signedIn is a started server and a client that has logged in as alice.
type signedIn struct {
	fix        *fixture
	srv        *running
	httpClient *http.Client
	token      string
}

func startSignedIn(t *testing.T) *signedIn {
	t.Helper()
	fix := newFixture(t)
	srv := start(t, fix)
	httpClient := client(nil)
	resp := call(t, httpClient, http.MethodPost, srv.url("/api/v1/login"), "", nil, goodLogin)
	var login wire.LoginResponse
	decode(t, resp.body, &login)
	if resp.StatusCode != http.StatusOK || login.Token == "" || !login.Expires.After(time.Now()) {
		t.Fatalf("login: status %d response %+v", resp.StatusCode, login)
	}
	return &signedIn{fix: fix, srv: srv, httpClient: httpClient, token: login.Token}
}

// request sends a request for path below the base path of the API, with the token of the
// session.
func (s *signedIn) request(t *testing.T, method, path string, header map[string]string) response {
	t.Helper()
	return call(t, s.httpClient, method, s.srv.url("/api"+path), s.token, header, "")
}

func TestHealth(t *testing.T) {
	t.Parallel()
	srv := start(t, newFixture(t))
	resp := call(t, client(nil), http.MethodGet, srv.url("/api/v1/health"), "", nil, "")
	var health wire.Health
	decode(t, resp.body, &health)
	if resp.StatusCode != http.StatusOK || health != (wire.Health{Status: "ok", Version: "test", Node: "east"}) {
		t.Errorf("status %d health %+v", resp.StatusCode, health)
	}
}

func TestLoginRefusesBadCredentials(t *testing.T) {
	t.Parallel()
	srv := start(t, newFixture(t))
	httpClient := client(nil)
	login := srv.url("/api/v1/login")
	resp := call(t, httpClient, http.MethodPost, login, "", nil, `{"user":"alice","password":"wrong"}`)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d", resp.StatusCode)
	}
	resp = call(t, httpClient, http.MethodPost, login, "", nil, `{"user":"nobody","password":"x"}`)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unknown user: status = %d", resp.StatusCode)
	}
}

func TestRequestsWithoutATokenAreRefused(t *testing.T) {
	t.Parallel()
	srv := start(t, newFixture(t))
	resp := call(t, client(nil), http.MethodGet, srv.url("/api/v1/tree/"), "", nil, "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestListingOfTheShares(t *testing.T) {
	t.Parallel()
	session := startSignedIn(t)
	var shares wire.Listing
	decode(t, session.request(t, http.MethodGet, "/v1/tree/", nil).body, &shares)
	if len(shares.Entries) != 1 || shares.Entries[0].Name != "media" || !shares.Entries[0].Dir {
		t.Errorf("shares = %+v", shares)
	}
}

func TestListingOfAFolderHidesDotFiles(t *testing.T) {
	t.Parallel()
	session := startSignedIn(t)
	var dir wire.Listing
	decode(t, session.request(t, http.MethodGet, "/v1/tree/media/", nil).body, &dir)
	names := make([]string, 0, len(dir.Entries))
	for _, entry := range dir.Entries {
		names = append(names, entry.Name)
	}
	if strings.Join(names, ",") != "hello.bin,page.html" {
		t.Errorf("names = %v (the dot file must be hidden)", names)
	}
	resp := session.request(t, http.MethodGet, "/v1/tree/media/.hidden", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("hidden file: status = %d", resp.StatusCode)
	}
}

func TestHeadReportsTheDigest(t *testing.T) {
	t.Parallel()
	session := startSignedIn(t)
	resp := session.request(t, http.MethodHead, "/v1/file/media/hello.bin", nil)
	sum := sha256.Sum256([]byte("0123456789"))
	for name, want := range map[string]string{
		"ETag":                share.ETag(10, fileTime()),
		"Content-Length":      "10",
		"Accept-Ranges":       "bytes",
		wire.HeaderHeadDigest: hex.EncodeToString(sum[:]),
	} {
		if got := resp.Header.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

func TestRangedGetThenTheFileIsReplaced(t *testing.T) {
	t.Parallel()
	session := startSignedIn(t)
	header := map[string]string{"Range": "bytes=2-5", "If-Match": share.ETag(10, fileTime())}
	resp := session.request(t, http.MethodGet, "/v1/file/media/hello.bin", header)
	if resp.StatusCode != http.StatusPartialContent || string(resp.body) != "2345" {
		t.Errorf("status %d body %q", resp.StatusCode, resp.body)
	}
	if got := resp.Header.Get("Content-Range"); got != "bytes 2-5/10" {
		t.Errorf("Content-Range = %q", got)
	}
	replaced := write(t, session.fix.shareDir, "hello.bin", "abcdefghijklmnop")
	setMTime(t, replaced, fileTime().Add(time.Hour))
	resp = session.request(t, http.MethodGet, "/v1/file/media/hello.bin", header)
	var failure wire.Error
	decode(t, resp.body, &failure)
	if resp.StatusCode != http.StatusPreconditionFailed || failure.Error == "" {
		t.Errorf("after replacement: status %d body %q", resp.StatusCode, resp.body)
	}
}

func TestCompressedGet(t *testing.T) {
	t.Parallel()
	session := startSignedIn(t)
	header := map[string]string{"Accept-Encoding": "gzip"}
	resp := session.request(t, http.MethodGet, "/v1/file/media/page.html", header)
	if resp.Header.Get("Content-Encoding") != "gzip" || resp.Header.Get("Vary") != "Accept-Encoding" {
		t.Fatalf("headers = %v", resp.Header)
	}
	reader, err := gzip.NewReader(bytes.NewReader(resp.body))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := io.ReadAll(reader)
	if err != nil || string(plain) != htmlContent() {
		t.Errorf("decompressed %d bytes, err %v", len(plain), err)
	}
	if len(resp.body) >= len(htmlContent()) {
		t.Errorf("compressed size %d is not smaller than %d", len(resp.body), len(htmlContent()))
	}
}

func TestSpace(t *testing.T) {
	t.Parallel()
	session := startSignedIn(t)
	var space wire.Space
	decode(t, session.request(t, http.MethodGet, "/v1/space/media", nil).body, &space)
	if space.Total == 0 || space.Free > space.Total {
		t.Errorf("space = %+v", space)
	}
}

func TestNodes(t *testing.T) {
	t.Parallel()
	session := startSignedIn(t)
	var nodes wire.Nodes
	decode(t, session.request(t, http.MethodGet, "/v1/nodes", nil).body, &nodes)
	if nodes.Cluster != "example" || len(nodes.Nodes) != 1 {
		t.Fatalf("nodes = %+v", nodes)
	}
	if nodes.Nodes[0].URL != "https://east.example.com/api" {
		t.Errorf("node URL = %q", nodes.Nodes[0].URL)
	}
}

func TestOutsideTheBasePathIsNotFound(t *testing.T) {
	t.Parallel()
	session := startSignedIn(t)
	resp := call(t, session.httpClient, http.MethodGet, session.srv.url("/v1/health"), "", nil, "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestShutdownClosesThePortAndTheLogHidesSecrets(t *testing.T) {
	t.Parallel()
	session := startSignedIn(t)
	call(t, session.httpClient, http.MethodPost, session.srv.url("/api/v1/login"), "", nil,
		`{"user":"alice","password":"wrong"}`)
	session.request(t, http.MethodGet, "/v1/health", nil)
	// Only a request with a valid token puts the user in the access log.
	session.request(t, http.MethodGet, "/v1/nodes", nil)

	if err := session.srv.stop(); err != nil {
		t.Fatalf("Run returned %v after cancellation, want nil", err)
	}
	if isOpen(session.srv.addrs[0].String()) {
		t.Error("the port is still open after Run returned")
	}
	logs := session.srv.logs.String()
	if !strings.Contains(logs, "path=/api/v1/health") || !strings.Contains(logs, "user=alice") {
		t.Errorf("access log is missing requests:\n%s", logs)
	}
	for _, secret := range []string{session.token, "right-password", "wrong"} {
		if strings.Contains(logs, secret) {
			t.Errorf("log contains %q", secret)
		}
	}
}

// listenerKinds is a server with one listener of each kind: plain HTTP, TLS with a
// configured certificate, and TLS with the server's own certificate.
type listenerKinds struct {
	srv        *running
	state      string
	configured *x509.CertPool // trusts the configured certificate
}

const (
	plainListener      = 0
	configuredListener = 1
	ownCertListener    = 2
)

func startListenerKinds(t *testing.T) listenerKinds {
	t.Helper()
	fix := newFixture(t)
	files := selfSigned(t)
	fix.cfg.Listen = []config.Endpoint{
		configEndpoint(nil, true),
		configEndpoint(&config.TLS{CertFile: files.certFile, KeyFile: files.keyFile}, false),
		configEndpoint(nil, false),
	}
	srv := start(t, fix)
	if len(srv.addrs) != 3 {
		t.Fatalf("Ready got %d addresses, want 3", len(srv.addrs))
	}
	return listenerKinds{srv: srv, state: fix.state, configured: files.pool}
}

// health requests the health of the listener at index, as a client that trusts pool.
func (k listenerKinds) health(t *testing.T, scheme string, index int, pool *x509.CertPool) response {
	t.Helper()
	url := scheme + "://" + k.srv.addrs[index].String() + "/api/v1/health"
	return call(t, client(pool), http.MethodGet, url, "", nil, "")
}

func TestPlainListener(t *testing.T) {
	t.Parallel()
	kinds := startListenerKinds(t)
	resp := kinds.health(t, "http", plainListener, nil)
	if resp.StatusCode != http.StatusOK || resp.TLS != nil {
		t.Errorf("plain listener: status %d, TLS %+v", resp.StatusCode, resp.TLS)
	}
}

func TestConfiguredCertificateListener(t *testing.T) {
	t.Parallel()
	kinds := startListenerKinds(t)
	resp := kinds.health(t, "https", configuredListener, kinds.configured)
	if resp.StatusCode != http.StatusOK || resp.TLS == nil || resp.TLS.Version < tls.VersionTLS12 {
		t.Errorf("configured certificate: status %d, TLS %+v", resp.StatusCode, resp.TLS)
	}
}

func TestListenerWithTheOwnCertificate(t *testing.T) {
	t.Parallel()
	kinds := startListenerKinds(t)
	own := trusting(t, filepath.Join(kinds.state, localDirName, tlsDirName, certFileName))
	resp := kinds.health(t, "https", ownCertListener, own)
	if resp.StatusCode != http.StatusOK || resp.TLS == nil {
		t.Errorf("own certificate: status %d, TLS %+v", resp.StatusCode, resp.TLS)
	}
}

func TestTLSListenerRefusesAClientThatDoesNotTrustTheCertificate(t *testing.T) {
	t.Parallel()
	kinds := startListenerKinds(t)
	url := "https://" + kinds.srv.addrs[configuredListener].String() + "/api/v1/health"
	untrusted, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client(nil).Do(untrusted)
	if err == nil {
		defer closeQuietly(resp.Body)
		t.Error("a client that does not trust the certificate was served")
	}
	if err := kinds.srv.stop(); err != nil {
		t.Errorf("Run = %v", err)
	}
}

func TestRunWithoutShares(t *testing.T) {
	t.Parallel()
	fix := newFixture(t)
	fix.cfg.Shares = map[string]config.Share{}
	srv := start(t, fix)
	httpClient := client(nil)
	var token wire.LoginResponse
	decode(t, call(t, httpClient, http.MethodPost, srv.url("/api/v1/login"), "", nil, goodLogin).body, &token)
	resp := call(t, httpClient, http.MethodGet, srv.url("/api/v1/tree/"), token.Token, nil, "")
	var shares wire.Listing
	decode(t, resp.body, &shares)
	if resp.StatusCode != http.StatusOK || len(shares.Entries) != 0 {
		t.Errorf("status %d, shares %+v", resp.StatusCode, shares)
	}
}

func TestRunWithoutLoggerOrReadyCallback(t *testing.T) {
	t.Parallel()
	fix := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Run(ctx, fix.cfg, fix.state, Options{}); err != nil {
		t.Errorf("Run = %v", err)
	}
}

func TestRunFailsAtStart(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "missing")
	tests := []struct {
		name   string
		mutate func(fix *fixture)
		want   string
	}{
		{"share path does not exist", func(fix *fixture) {
			fix.cfg.Shares["zzz"] = config.Share{Path: missing}
		}, `share "zzz"`},
		{"node id file is malformed", func(fix *fixture) {
			write(t, fix.state, "local/node-id", "not an id\n")
		}, "node id file"},
		{"signing key file is invalid", func(fix *fixture) {
			write(t, fix.state, "local/node-id", "0123456789abcdef\n")
			write(t, fix.state, "shared/keys/0123456789abcdef.yaml", "keys: []\n")
		}, "signing key file"},
		{"node file cannot be written", func(fix *fixture) {
			// A folder where this node's file belongs cannot be replaced by the file.
			write(t, fix.state, "local/node-id", "0123456789abcdef\n")
			node := filepath.Join(fix.state, "shared", "nodes", datadir.NodeFile("0123456789abcdef"))
			if err := os.MkdirAll(node, 0o755); err != nil {
				t.Fatal(err)
			}
		}, "write node file"},
		{"state folder is a file", func(fix *fixture) {
			write(t, fix.state, "shared/keys", "a file where a folder belongs")
		}, "is not a folder"},
		{"state folders cannot be made", func(fix *fixture) {
			parent := write(t, t.TempDir(), "file", "a file where a folder belongs")
			fix.state = filepath.Join(parent, "state")
		}, "create folder"},
		{"certificate is missing", func(fix *fixture) {
			fix.cfg.Listen[0] = configEndpoint(&config.TLS{CertFile: missing, KeyFile: missing}, false)
		}, "load certificate"},
		{"own certificate cannot be made", func(fix *fixture) {
			write(t, fix.state, "local/tls", "a file where a folder belongs")
			fix.cfg.Listen[0] = configEndpoint(nil, false)
		}, "self-signed certificate"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fix := newFixture(t)
			tt.mutate(fix)
			ready := false
			opts := Options{Ready: func([]net.Addr) { ready = true }}
			err := Run(context.Background(), fix.cfg, fix.state, opts)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Run = %v, want an error containing %q", err, tt.want)
			}
			if ready {
				t.Error("Ready was called")
			}
		})
	}
}

func TestRunFailsWhenAPortIsTaken(t *testing.T) {
	t.Parallel()
	fix := newFixture(t)
	var listenConfig net.ListenConfig
	taken, err := listenConfig.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeQuietly(taken) })
	free, err := listenConfig.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	freePort := portOf(t, free.Addr())
	closeQuietly(free)

	loopback := netip.MustParseAddr("127.0.0.1")
	takenPort := portOf(t, taken.Addr())
	fix.cfg.Listen = []config.Endpoint{
		{Listener: config.Listener{Address: loopback, Port: freePort}, Plain: true},
		{Listener: config.Listener{Address: loopback, Port: takenPort}, Plain: true},
	}
	opts := Options{Ready: func([]net.Addr) { t.Error("Ready was called") }}
	err = Run(context.Background(), fix.cfg, fix.state, opts)
	if err == nil || !strings.Contains(err.Error(), "listen on 127.0.0.1:") {
		t.Fatalf("Run = %v", err)
	}
	if isOpen(net.JoinHostPort("127.0.0.1", strconv.Itoa(freePort))) {
		t.Error("the listener opened before the failure was left open")
	}
}
