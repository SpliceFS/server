package app

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SpliceFS/server/cluster"
	"github.com/SpliceFS/server/config"
)

// A client of a cluster trusts a server whose certificate the cluster key signed and that is
// valid now, and no other. This is the rule of the cluster key option of remote.TLSConfig in
// the common module, which is not in a released version yet, so the test states the rule
// itself; it is replaced by that function when the module is released.
func clusterTrustingConfig(key ed25519.PublicKey, now func() time.Time) *tls.Config {
	return &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true,
		VerifyConnection: func(state tls.ConnectionState) error {
			leaf := state.PeerCertificates[0]
			if leaf.SignatureAlgorithm != x509.PureEd25519 ||
				!ed25519.Verify(key, leaf.RawTBSCertificate, leaf.Signature) {
				return errors.New("not signed by the cluster key")
			}
			if at := now(); at.Before(leaf.NotBefore) || at.After(leaf.NotAfter) {
				return errors.New("outside its validity time")
			}
			return nil
		},
	}
}

// ticker is a clock that a test moves: the time the server reads, and the daily check that it
// sends.
type ticker struct {
	now   atomic.Pointer[time.Time]
	ticks chan time.Time
}

func newTicker(start time.Time) *ticker {
	c := &ticker{ticks: make(chan time.Time)}
	c.now.Store(&start)
	return c
}

func (c *ticker) read() time.Time { return *c.now.Load() }

func (c *ticker) advance(by time.Duration) {
	next := c.read().Add(by)
	c.now.Store(&next)
}

func (c *ticker) clock() clock {
	return clock{
		now:   c.read,
		daily: func() (<-chan time.Time, func()) { return c.ticks, func() {} },
	}
}

// clusterServer is a server with a cluster key and a listener that uses the generated
// certificate.
type clusterServer struct {
	*reloading

	key   cluster.Key
	clock *ticker
	fix   *fixture
}

func startClusterServer(t *testing.T) clusterServer {
	t.Helper()
	fix := newFixture(t)
	key := testClusterKey(t)
	write(t, fix.data.clusterDir, cluster.KeyFile, string(cluster.MarshalKey(key)))
	fix.cfg.Listen = []config.Endpoint{configEndpoint(nil, false)}
	clk := newTicker(time.Now().Truncate(time.Second))
	reloader := newReloading()
	reloader.running = startWith(t, fix, func(opts *Options) {
		opts.clock = clk.clock()
		opts.Reload = reloader.signals
		opts.Load = func() (*config.Config, error) { return (<-reloader.loads).cfg, nil }
		opts.Applied = func(cfg *config.Config) { reloader.applied <- cfg }
	})
	return clusterServer{reloading: reloader, key: key, clock: clk, fix: fix}
}

// connect makes a TLS connection to the server as a client with trust and returns the
// certificate the server presented.
func (s clusterServer) connect(t *testing.T, trust *tls.Config) (*x509.Certificate, error) {
	t.Helper()
	dialer := tls.Dialer{Config: trust}
	conn, err := dialer.DialContext(context.Background(), "tcp", s.addrs[0])
	if err != nil {
		return nil, err
	}
	defer closeQuietly(conn)
	// A tls.Dialer returns a *tls.Conn.
	return conn.(*tls.Conn).ConnectionState().PeerCertificates[0], nil //nolint:forcetypeassert,errcheck // see above
}

func (s clusterServer) trust() *tls.Config {
	return clusterTrustingConfig(ed25519.PublicKey(s.key.Private[ed25519.SeedSize:]), s.clock.read)
}

func TestClientWithTheClusterKeyTrustsTheServer(t *testing.T) {
	t.Parallel()
	server := startClusterServer(t)
	leaf, err := server.connect(t, server.trust())
	if err != nil {
		t.Fatalf("a client with the cluster key does not trust the server: %v", err)
	}
	if !signedBy(leaf, server.key) {
		t.Error("the served certificate is not signed by the cluster key")
	}
	// The server also serves requests over it.
	resp := call(t, &http.Client{Transport: &http.Transport{
		TLSClientConfig: server.trust(), DisableKeepAlives: true,
	}}, http.MethodGet, "https://"+server.addrs[0]+"/api/v1/health", "", nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status %d", resp.StatusCode)
	}
}

func TestClientWithAnotherKeyDoesNotTrustTheServer(t *testing.T) {
	t.Parallel()
	server := startClusterServer(t)
	other := testClusterKey(t)
	trust := clusterTrustingConfig(ed25519.PublicKey(other.Private[ed25519.SeedSize:]), server.clock.read)
	if _, err := server.connect(t, trust); err == nil {
		t.Error("a client with another cluster key trusts the server")
	}
}

func TestClientDoesNotTrustAnExpiredCertificate(t *testing.T) {
	t.Parallel()
	server := startClusterServer(t)
	later := func() time.Time { return server.clock.read().Add(91 * 24 * time.Hour) }
	trust := clusterTrustingConfig(ed25519.PublicKey(server.key.Private[ed25519.SeedSize:]), later)
	if _, err := server.connect(t, trust); err == nil {
		t.Error("a client trusts a certificate after it expired")
	}
}

// The daily check renews the certificate, and the next connection already gets it.
func TestDailyCheckRenewsTheServedCertificate(t *testing.T) {
	t.Parallel()
	server := startClusterServer(t)
	before, err := server.connect(t, server.trust())
	if err != nil {
		t.Fatal(err)
	}
	server.clock.advance(61 * 24 * time.Hour)
	server.clock.ticks <- server.clock.read()
	server.logs.waitFor(t, "node certificate issued", 2)
	after, err := server.connect(t, server.trust())
	if err != nil {
		t.Fatal(err)
	}
	if before.SerialNumber.Cmp(after.SerialNumber) == 0 || !after.NotBefore.Equal(server.clock.read()) {
		t.Errorf("the new connection got the old certificate: serial %v, valid from %v",
			after.SerialNumber, after.NotBefore)
	}
}

// A check that fails leaves the served certificate, and says so.
func TestDailyCheckThatFailsKeepsTheCertificate(t *testing.T) {
	t.Parallel()
	server := startClusterServer(t)
	before, err := server.connect(t, server.trust())
	if err != nil {
		t.Fatal(err)
	}
	server.clock.advance(61 * 24 * time.Hour)
	// A folder where cert.pem belongs makes writing the renewed certificate fail.
	certPath := filepath.Join(server.fix.data.tlsDir, certFileName)
	replaceWithFolder(t, certPath)
	server.clock.ticks <- server.clock.read()
	server.logs.waitFor(t, "cannot renew the node certificate", 1)
	server.clock.advance(-61 * 24 * time.Hour)
	after, err := server.connect(t, server.trust())
	if err != nil || before.SerialNumber.Cmp(after.SerialNumber) != 0 {
		t.Errorf("the certificate changed after a failed check (error %v)", err)
	}
}

// A reload that changes the names makes a certificate with them, served to the next connection.
func TestReloadWithChangedNamesServesANewCertificate(t *testing.T) {
	t.Parallel()
	server := startClusterServer(t)
	cfg := *server.fix.cfg
	cfg.Cluster.Node = "north"
	server.apply(t, &cfg)
	leaf, err := server.connect(t, server.trust())
	if err != nil {
		t.Fatal(err)
	}
	if !signedBy(leaf, server.key) || !containsName(leaf, "north") {
		t.Errorf("the certificate after the reload names %v", certNamesOf(leaf))
	}
}

// A reload whose certificate cannot be made is not applied.
func TestReloadThatCannotMakeTheCertificateIsRefused(t *testing.T) {
	t.Parallel()
	server := startClusterServer(t)
	replaceWithFolder(t, filepath.Join(server.fix.data.tlsDir, certFileName))
	cfg := *server.fix.cfg
	cfg.Cluster.Node = "north"
	if err := server.refuse(t, loaded{cfg: &cfg}); err == nil {
		t.Error("no refusal")
	}
	leaf, err := server.connect(t, server.trust())
	if err != nil || containsName(leaf, "north") {
		t.Errorf("the server changed although the reload was refused (error %v)", err)
	}
}

func containsName(leaf *x509.Certificate, name string) bool {
	return slices.Contains(leaf.DNSNames, name)
}

// A server whose start cannot make its certificate does not start.
func TestRunFailsWhenTheNodeCertificateCannotBeMade(t *testing.T) {
	t.Parallel()
	fix := newFixture(t)
	write(t, fix.data.clusterDir, cluster.KeyFile, string(cluster.MarshalKey(testClusterKey(t))))
	fix.cfg.Listen = []config.Endpoint{configEndpoint(nil, false)}
	write(t, filepath.Dir(fix.data.tlsDir), tlsDirName, "a file where the folder belongs")
	givePorts(t, fix)
	err := Run(context.Background(), fix.cfg, fix.state, Options{})
	if err == nil {
		t.Fatal("Run started")
	}
}

// replaceWithFolder removes what is at path and makes a folder there with something in it, so
// that a file cannot be written at path, by any user.
func replaceWithFolder(t *testing.T, path string) {
	t.Helper()
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(path, "inside"), 0o700); err != nil {
		t.Fatal(err)
	}
}
