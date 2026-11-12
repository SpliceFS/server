package app

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
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
func call(t *testing.T, c *http.Client, method, url, token string, header map[string]string, body string) (response, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response{resp}, data
}

func decode(t *testing.T, data []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("%v in %q", err, data)
	}
}

func TestEndToEnd(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	srv := start(t, f)
	c := client(nil)
	api := func(path string) string { return srv.url("/api" + path) }

	t.Run("health", func(t *testing.T) {
		resp, data := call(t, c, "GET", api("/v1/health"), "", nil, "")
		var h wire.Health
		decode(t, data, &h)
		if resp.StatusCode != 200 || h != (wire.Health{Status: "ok", Version: "test", Node: "east"}) {
			t.Errorf("status %d health %+v", resp.StatusCode, h)
		}
	})

	t.Run("login with a bad password", func(t *testing.T) {
		resp, _ := call(t, c, "POST", api("/v1/login"), "", nil, `{"user":"alice","password":"wrong"}`)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("status = %d", resp.StatusCode)
		}
		resp, _ = call(t, c, "POST", api("/v1/login"), "", nil, `{"user":"nobody","password":"x"}`)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("unknown user: status = %d", resp.StatusCode)
		}
	})

	resp, data := call(t, c, "POST", api("/v1/login"), "", nil, `{"user":"alice","password":"right-password"}`)
	var login wire.LoginResponse
	decode(t, data, &login)
	if resp.StatusCode != 200 || login.Token == "" || !login.Expires.After(time.Now()) {
		t.Fatalf("login: status %d response %+v", resp.StatusCode, login)
	}
	token := login.Token

	t.Run("requests without a token are refused", func(t *testing.T) {
		resp, _ := call(t, c, "GET", api("/v1/tree/"), "", nil, "")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("status = %d", resp.StatusCode)
		}
	})

	t.Run("listing", func(t *testing.T) {
		_, data := call(t, c, "GET", api("/v1/tree/"), token, nil, "")
		var shares wire.Listing
		decode(t, data, &shares)
		if len(shares.Entries) != 1 || shares.Entries[0].Name != "media" || !shares.Entries[0].Dir {
			t.Errorf("shares = %+v", shares)
		}
		_, data = call(t, c, "GET", api("/v1/tree/media/"), token, nil, "")
		var dir wire.Listing
		decode(t, data, &dir)
		names := make([]string, 0, len(dir.Entries))
		for _, e := range dir.Entries {
			names = append(names, e.Name)
		}
		if strings.Join(names, ",") != "hello.bin,page.html" {
			t.Errorf("names = %v (the dot file must be hidden)", names)
		}
		resp, _ := call(t, c, "GET", api("/v1/tree/media/.hidden"), token, nil, "")
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("hidden file: status = %d", resp.StatusCode)
		}
	})

	etag := share.ETag(10, fileTime)
	t.Run("head with digest", func(t *testing.T) {
		resp, _ := call(t, c, "HEAD", api("/v1/file/media/hello.bin"), token, nil, "")
		sum := sha256.Sum256([]byte("0123456789"))
		for name, want := range map[string]string{
			"ETag":                etag,
			"Content-Length":      "10",
			"Accept-Ranges":       "bytes",
			wire.HeaderHeadDigest: hex.EncodeToString(sum[:]),
		} {
			if got := resp.Header.Get(name); got != want {
				t.Errorf("%s = %q, want %q", name, got, want)
			}
		}
	})

	t.Run("ranged get, then the file is replaced", func(t *testing.T) {
		header := map[string]string{"Range": "bytes=2-5", "If-Match": etag}
		resp, data := call(t, c, "GET", api("/v1/file/media/hello.bin"), token, header, "")
		if resp.StatusCode != http.StatusPartialContent || string(data) != "2345" {
			t.Errorf("status %d body %q", resp.StatusCode, data)
		}
		if got := resp.Header.Get("Content-Range"); got != "bytes 2-5/10" {
			t.Errorf("Content-Range = %q", got)
		}
		replaced := write(t, f.shareDir, "hello.bin", "abcdefghijklmnop")
		setMTime(t, replaced, fileTime.Add(time.Hour))
		resp, data = call(t, c, "GET", api("/v1/file/media/hello.bin"), token, header, "")
		var e wire.Error
		decode(t, data, &e)
		if resp.StatusCode != http.StatusPreconditionFailed || e.Error == "" {
			t.Errorf("after replacement: status %d body %q", resp.StatusCode, data)
		}
	})

	t.Run("compressed get", func(t *testing.T) {
		resp, data := call(t, c, "GET", api("/v1/file/media/page.html"), token, map[string]string{"Accept-Encoding": "gzip"}, "")
		if resp.Header.Get("Content-Encoding") != "gzip" || resp.Header.Get("Vary") != "Accept-Encoding" {
			t.Fatalf("headers = %v", resp.Header)
		}
		zr, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		plain, err := io.ReadAll(zr)
		if err != nil || string(plain) != htmlContent {
			t.Errorf("decompressed %d bytes, err %v", len(plain), err)
		}
		if len(data) >= len(htmlContent) {
			t.Errorf("compressed size %d is not smaller than %d", len(data), len(htmlContent))
		}
	})

	t.Run("space", func(t *testing.T) {
		_, data := call(t, c, "GET", api("/v1/space/media"), token, nil, "")
		var sp wire.Space
		decode(t, data, &sp)
		if sp.Total == 0 || sp.Free > sp.Total {
			t.Errorf("space = %+v", sp)
		}
	})

	t.Run("nodes", func(t *testing.T) {
		_, data := call(t, c, "GET", api("/v1/nodes"), token, nil, "")
		var nodes wire.Nodes
		decode(t, data, &nodes)
		if nodes.Cluster != "example" || len(nodes.Nodes) != 1 || nodes.Nodes[0].URL != "https://east.example.com/api" {
			t.Errorf("nodes = %+v", nodes)
		}
	})

	t.Run("outside the base path", func(t *testing.T) {
		resp, _ := call(t, c, "GET", srv.url("/v1/health"), "", nil, "")
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("status = %d", resp.StatusCode)
		}
	})

	if err := srv.stop(); err != nil {
		t.Fatalf("Run returned %v after cancellation, want nil", err)
	}
	if isOpen(srv.addrs[0].String()) {
		t.Error("the port is still open after Run returned")
	}
	logs := srv.logs.String()
	if !strings.Contains(logs, "path=/api/v1/health") || !strings.Contains(logs, "user=alice") {
		t.Errorf("access log is missing requests:\n%s", logs)
	}
	for _, secret := range []string{token, "right-password", "wrong"} {
		if strings.Contains(logs, secret) {
			t.Errorf("log contains %q", secret)
		}
	}
}

func TestListenerKinds(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	certFile, keyFile, pool := selfSigned(t)
	f.cfg.Listen = []config.Endpoint{
		configEndpoint(nil, true),
		configEndpoint(&config.TLS{CertFile: certFile, KeyFile: keyFile}, false),
		configEndpoint(nil, false),
	}
	srv := start(t, f)
	if len(srv.addrs) != 3 {
		t.Fatalf("Ready got %d addresses, want 3", len(srv.addrs))
	}
	health := func(scheme string, i int) string {
		return scheme + "://" + srv.addrs[i].String() + "/api/v1/health"
	}

	resp, _ := call(t, client(nil), "GET", health("http", 0), "", nil, "")
	if resp.StatusCode != 200 || resp.TLS != nil {
		t.Errorf("plain listener: status %d, TLS %+v", resp.StatusCode, resp.TLS)
	}
	resp, _ = call(t, client(pool), "GET", health("https", 1), "", nil, "")
	if resp.StatusCode != 200 || resp.TLS == nil || resp.TLS.Version < 0x0303 {
		t.Errorf("configured certificate: status %d, TLS %+v", resp.StatusCode, resp.TLS)
	}
	ownCert := filepath.Join(f.state, localDirName, tlsDirName, certFileName)
	resp, _ = call(t, client(trusting(t, ownCert)), "GET", health("https", 2), "", nil, "")
	if resp.StatusCode != 200 || resp.TLS == nil {
		t.Errorf("own certificate: status %d, TLS %+v", resp.StatusCode, resp.TLS)
	}
	untrusted, err := http.NewRequestWithContext(
		context.Background(), "GET", health("https", 1), nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp, err := client(nil).Do(untrusted); err == nil {
		_ = resp.Body.Close()
		t.Error("a client that does not trust the certificate was served")
	}
	if err := srv.stop(); err != nil {
		t.Errorf("Run = %v", err)
	}
}

func TestRunWithoutShares(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.cfg.Shares = map[string]config.Share{}
	srv := start(t, f)
	c := client(nil)
	login := `{"user":"alice","password":"right-password"}`
	_, data := call(t, c, "POST", srv.url("/api/v1/login"), "", nil, login)
	var token wire.LoginResponse
	decode(t, data, &token)
	resp, data := call(t, c, "GET", srv.url("/api/v1/tree/"), token.Token, nil, "")
	var shares wire.Listing
	decode(t, data, &shares)
	if resp.StatusCode != 200 || len(shares.Entries) != 0 {
		t.Errorf("status %d, shares %+v", resp.StatusCode, shares)
	}
}

func TestRunWithoutLoggerOrReadyCallback(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Run(ctx, f.cfg, f.state, Options{}); err != nil {
		t.Errorf("Run = %v", err)
	}
}

func TestRunFailsAtStart(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "missing")
	tests := []struct {
		name   string
		mutate func(f *fixture)
		want   string
	}{
		{"share path does not exist", func(f *fixture) {
			f.cfg.Shares["zzz"] = config.Share{Path: missing}
		}, `share "zzz"`},
		{"node id file is malformed", func(f *fixture) {
			write(t, f.state, "local/node-id", "not an id\n")
		}, "node id file"},
		{"signing key file is invalid", func(f *fixture) {
			write(t, f.state, "local/node-id", "0123456789abcdef\n")
			write(t, f.state, "shared/keys/0123456789abcdef.yaml", "keys: []\n")
		}, "signing key file"},
		{"node file cannot be written", func(f *fixture) {
			// A folder where this node's file belongs cannot be replaced by the file.
			write(t, f.state, "local/node-id", "0123456789abcdef\n")
			node := filepath.Join(f.state, "shared/nodes", datadir.NodeFile("0123456789abcdef"))
			if err := os.MkdirAll(node, 0o755); err != nil {
				t.Fatal(err)
			}
		}, "write node file"},
		{"state folder is a file", func(f *fixture) {
			write(t, f.state, "shared/keys", "a file where a folder belongs")
		}, "is not a folder"},
		{"state folders cannot be made", func(f *fixture) {
			parent := write(t, t.TempDir(), "file", "a file where a folder belongs")
			f.state = filepath.Join(parent, "state")
		}, "create folder"},
		{"certificate is missing", func(f *fixture) {
			f.cfg.Listen[0] = configEndpoint(&config.TLS{CertFile: missing, KeyFile: missing}, false)
		}, "load certificate"},
		{"own certificate cannot be made", func(f *fixture) {
			write(t, f.state, "local/tls", "a file where a folder belongs")
			f.cfg.Listen[0] = configEndpoint(nil, false)
		}, "self-signed certificate"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			tt.mutate(f)
			ready := false
			opts := Options{Ready: func([]net.Addr) { ready = true }}
			err := Run(context.Background(), f.cfg, f.state, opts)
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
	f := newFixture(t)
	var lc net.ListenConfig
	taken, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = taken.Close() })
	free, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	freePort := free.Addr().(*net.TCPAddr).Port
	_ = free.Close()

	loopback := netip.MustParseAddr("127.0.0.1")
	takenPort := taken.Addr().(*net.TCPAddr).Port
	f.cfg.Listen = []config.Endpoint{
		{Listener: config.Listener{Address: loopback, Port: freePort}, Plain: true},
		{Listener: config.Listener{Address: loopback, Port: takenPort}, Plain: true},
	}
	opts := Options{Ready: func([]net.Addr) { t.Error("Ready was called") }}
	err = Run(context.Background(), f.cfg, f.state, opts)
	if err == nil || !strings.Contains(err.Error(), "listen on 127.0.0.1:") {
		t.Fatalf("Run = %v", err)
	}
	if isOpen(net.JoinHostPort("127.0.0.1", strconv.Itoa(freePort))) {
		t.Error("the listener opened before the failure was left open")
	}
}
