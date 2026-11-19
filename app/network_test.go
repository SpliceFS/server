package app

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/SpliceFS/common/remote"

	"github.com/SpliceFS/server/config"
)

// These tests run the real server (state folder, users file, keys file, share folders on
// disk, a TLS listener with the certificate the server makes for itself) and drive it with
// the remote client of common over a real TCP connection. The protocol itself is tested in
// common; these prove the wiring of the server and its file system behind it.

// networkClientVersion is a build without a release number, which the version rule lets
// through: the rule is tested in common, not here.
const networkClientVersion = "dev"

// networkFixture is a fixture whose only listener serves TLS with the server's own
// certificate, with the given settings and the given keys added to the share media.
func networkFixture(t *testing.T, settings, shareSettings string) *fixture {
	t.Helper()
	fix := newFixture(t)
	fix.cfg = loadConfig(t,
		map[string]string{"10-test.yaml": settings},
		map[string]string{"media.yaml": "path: " + fix.shareDir + "\n" + shareSettings})
	fix.cfg.Listen = []config.Endpoint{configEndpoint(nil, false)}
	return fix
}

// forwarding is a proxy in front of the server: it says that every request comes from
// address.
type forwarding struct {
	next    http.RoundTripper
	address string
}

func (f forwarding) RoundTrip(req *http.Request) (*http.Response, error) {
	forwarded := req.Clone(req.Context())
	forwarded.Header.Set("X-Forwarded-For", f.address)
	return f.next.RoundTrip(forwarded)
}

// remoteNode returns a remote client of the running server that trusts the certificate the
// server made. wrap, if set, puts something in front of the TLS transport, as a proxy is.
func remoteNode(t *testing.T, fix *fixture, srv *running, wrap func(http.RoundTripper) http.RoundTripper) *remote.Node {
	t.Helper()
	pool := trusting(t, filepath.Join(fix.state, localDirName, tlsDirName, certFileName))
	var transport http.RoundTripper = &http.Transport{TLSClientConfig: tlsConfig(pool), DisableKeepAlives: true}
	if wrap != nil {
		transport = wrap(transport)
	}
	httpClient := &http.Client{Transport: transport, Timeout: testTimeout}
	node, err := remote.New("east", "https://"+srv.addrs[0]+"/api", networkClientVersion, httpClient)
	if err != nil {
		t.Fatal(err)
	}
	return node
}

// remoteLogin logs alice in and returns her token.
func remoteLogin(t *testing.T, node *remote.Node) string {
	t.Helper()
	resp, err := node.Login(t.Context(), "alice", "right-password")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	return resp.Token
}

// names returns the names of the entries of dir.
func names(t *testing.T, node *remote.Node, token, dir string) []string {
	t.Helper()
	entries, err := node.List(t.Context(), token, dir)
	if err != nil {
		t.Fatalf("list %q: %v", dir, err)
	}
	listed := make([]string, 0, len(entries))
	for _, entry := range entries {
		listed = append(listed, entry.Name)
	}
	return listed
}

// readRange returns length bytes of file from off, pinned to etag.
func readRange(t *testing.T, node *remote.Node, token, file, etag string, off, length int64) ([]byte, error) {
	t.Helper()
	body, err := node.Range(t.Context(), token, file, etag, off, length)
	if err != nil {
		return nil, err
	}
	defer closeQuietly(body)
	return io.ReadAll(body)
}

// TestNetworkNamesTheServerHides is the cluster scenario "names the server hides": names
// that start with a dot, symbolic links, links that leave the share and files younger than
// min_age are neither listed nor served, and the plain files are served byte for byte.
func TestNetworkNamesTheServerHides(t *testing.T) {
	t.Parallel()
	fix := networkFixture(t, testSettings, "min_age: 1h\n")
	plain := map[string]string{
		".sync-tool.movie.tmp": "the temporary file of a transfer tool",
		"real.txt":             "the file the link points to",
		"movie.part":           "a name that only looks like a partial file",
	}
	for name, content := range plain {
		setMTime(t, write(t, fix.shareDir, name, content), fileTime())
	}
	outside := write(t, filepath.Dir(fix.shareDir), "outside.txt", "not in the share")
	// A modification time in the future is younger than any min_age, whatever the clock says.
	setMTime(t, write(t, fix.shareDir, "fresh.bin", "still being written"), time.Now().Add(24*time.Hour))
	for link, target := range map[string]string{"link.txt": "real.txt", "escape.txt": outside} {
		if err := os.Symlink(target, filepath.Join(fix.shareDir, link)); err != nil {
			t.Fatal(err)
		}
	}
	srv := start(t, fix)
	node := remoteNode(t, fix, srv, nil)
	token := remoteLogin(t, node)

	want := []string{"hello.bin", "movie.part", "page.html", "real.txt"}
	if got := names(t, node, token, "media"); !slices.Equal(got, want) {
		t.Errorf("media lists %q, want %q", got, want)
	}
	hidden := []string{".hidden", ".sync-tool.movie.tmp", "link.txt", "escape.txt", "fresh.bin"}
	for _, name := range hidden {
		if _, err := node.Head(t.Context(), token, "media/"+name); !errors.Is(err, remote.ErrNotFound) {
			t.Errorf("head %s: err = %v, want remote.ErrNotFound", name, err)
		}
	}
	meta, err := node.Head(t.Context(), token, "media/real.txt")
	if err != nil {
		t.Fatal(err)
	}
	got, err := readRange(t, node, token, "media/real.txt", meta.ETag, 0, meta.Size)
	if err != nil || string(got) != "the file the link points to" {
		t.Errorf("range of real.txt = %q, %v", got, err)
	}
}

// TestNetworkFileChangedOnDiskIsRefused: a range pinned to the version of a file that has
// since been replaced on disk is refused (412), and a new head gives the new version.
func TestNetworkFileChangedOnDiskIsRefused(t *testing.T) {
	t.Parallel()
	fix := networkFixture(t, testSettings, "")
	srv := start(t, fix)
	node := remoteNode(t, fix, srv, nil)
	token := remoteLogin(t, node)
	before, err := node.Head(t.Context(), token, "media/hello.bin")
	if err != nil {
		t.Fatal(err)
	}

	path := write(t, fix.shareDir, "hello.bin", "a longer content than before")
	setMTime(t, path, fileTime().Add(time.Hour))

	_, err = readRange(t, node, token, "media/hello.bin", before.ETag, 0, 4)
	if !errors.Is(err, remote.ErrChanged) {
		t.Errorf("range of the old version: err = %v, want remote.ErrChanged", err)
	}
	after, err := node.Head(t.Context(), token, "media/hello.bin")
	if err != nil {
		t.Fatal(err)
	}
	if after.ETag == before.ETag || after.HeadDigest == before.HeadDigest || after.Size != 28 {
		t.Errorf("head after the change = %+v, before %+v", after, before)
	}
	got, err := readRange(t, node, token, "media/hello.bin", after.ETag, 0, after.Size)
	if err != nil || string(got) != "a longer content than before" {
		t.Errorf("range of the new version = %q, %v", got, err)
	}
}

// TestNetworkReloadKeepsAHeldToken: a token issued before a reload still works after it,
// and the reloaded configuration (a second share) is what it is checked against.
func TestNetworkReloadKeepsAHeldToken(t *testing.T) {
	t.Parallel()
	fix := networkFixture(t, testSettings, "")
	docs := filepath.Join(filepath.Dir(fix.shareDir), "docs")
	write(t, docs, "readme.txt", "docs")
	write(t, fix.data.usersDir, "alice.yaml",
		"password_hash: \""+lowCostHash("right-password")+"\"\nshares: [media, docs]\n")
	reloader := startReloading(t, fix)
	node := remoteNode(t, fix, reloader.running, nil)
	token := remoteLogin(t, node)
	if got := names(t, node, token, ""); !slices.Equal(got, []string{"media"}) {
		t.Fatalf("shares before the reload: %q", got)
	}

	next := loadConfig(t,
		map[string]string{"10-test.yaml": testSettings},
		map[string]string{"media.yaml": "path: " + fix.shareDir + "\n", "docs.yaml": "path: " + docs + "\n"})
	next.Listen = fix.cfg.Listen
	reloader.apply(t, next)

	if got := names(t, node, token, ""); !slices.Equal(got, []string{"docs", "media"}) {
		t.Errorf("shares after the reload with the held token: %q", got)
	}
	if got := names(t, node, token, "docs"); !slices.Equal(got, []string{"readme.txt"}) {
		t.Errorf("docs after the reload: %q", got)
	}
}

// TestNetworkRateLimitsTheAddressATrustedProxyForwards: behind a trusted proxy the login
// limit counts the address the proxy forwards, not the proxy's own, so one client used up
// leaves another able to log in.
func TestNetworkRateLimitsTheAddressATrustedProxyForwards(t *testing.T) {
	t.Parallel()
	// One login per hour: the window cannot end while the test runs.
	settings := "cluster:\n  name: example\n  node: east\n" +
		"auth:\n  login_rate: 1/h\ntrusted_proxies: [127.0.0.1]\n"
	fix := networkFixture(t, settings, "")
	srv := start(t, fix)
	from := func(address string) *remote.Node {
		return remoteNode(t, fix, srv, func(next http.RoundTripper) http.RoundTripper {
			return forwarding{next: next, address: address}
		})
	}
	first, second := from("192.0.2.1"), from("192.0.2.2")

	remoteLogin(t, first)
	if _, err := first.Login(t.Context(), "alice", "right-password"); !errors.Is(err, remote.ErrRateLimited) {
		t.Errorf("second login from 192.0.2.1: err = %v, want remote.ErrRateLimited", err)
	}
	remoteLogin(t, second)
}
