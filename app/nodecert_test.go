package app

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	"github.com/SpliceFS/server/cluster"
	"github.com/SpliceFS/server/config"
)

// testClusterKey makes a cluster key.
func testClusterKey(t *testing.T) cluster.Key {
	t.Helper()
	key, err := cluster.NewKey(certTime(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// keyHolder is the cluster key that a test lets the node read, and may change or remove.
type keyHolder struct {
	key atomic.Pointer[cluster.Key]
}

func (h *keyHolder) set(key cluster.Key) { h.key.Store(&key) }
func (h *keyHolder) clear()              { h.key.Store(nil) }

func (h *keyHolder) current() (cluster.Key, bool) {
	if key := h.key.Load(); key != nil {
		return *key, true
	}
	return cluster.Key{}, false
}

// nodeCertTest is a node certificate with a clock and a host name that the test moves.
type nodeCertTest struct {
	cert *nodeCert
	keys *keyHolder
	now  time.Time
	host string
	logs *syncBuffer
}

func newNodeCertTest(t *testing.T) *nodeCertTest {
	t.Helper()
	test := &nodeCertTest{keys: &keyHolder{}, now: certTime(), host: "test-host", logs: newSyncBuffer()}
	src := certSource{
		now:      func() time.Time { return test.now },
		hostname: func() (string, error) { return test.host, nil },
		random:   rand.Reader,
	}
	logger := slog.New(slog.NewTextHandler(test.logs, nil))
	test.cert = newNodeCert(certDir(t), src, test.keys.current, logger)
	return test
}

// listenOn is a configuration with a listener on each address that uses the generated
// certificate, and the cluster settings of the arguments.
func listenOn(url, node string, addresses ...string) *config.Config {
	cfg := &config.Config{Cluster: config.Cluster{Name: "example", Node: node, URL: url}}
	for _, address := range addresses {
		entry := configEndpoint(nil, false)
		entry.Address = netip.MustParseAddr(address)
		cfg.Listen = append(cfg.Listen, entry)
	}
	return cfg
}

// standardConfig is the configuration most tests of the node certificate use.
func standardConfig() *config.Config {
	return listenOn("https://east.example.com/api", "east", "192.0.2.7")
}

// served returns the certificate the node serves to a new connection.
func (n *nodeCertTest) served(t *testing.T) *x509.Certificate {
	t.Helper()
	cert, err := n.cert.get(nil)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return leaf
}

// signedBy reports whether the cluster key signed the leaf.
func signedBy(leaf *x509.Certificate, key cluster.Key) bool {
	return leaf.SignatureAlgorithm == x509.PureEd25519 &&
		ed25519.Verify(ed25519.PublicKey(key.Private[ed25519.SeedSize:]), leaf.RawTBSCertificate, leaf.Signature)
}

// publicKeyOf returns the ECDSA public key of leaf, and an empty key when it has another kind.
func publicKeyOf(leaf *x509.Certificate) *ecdsa.PublicKey {
	pub, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		return &ecdsa.PublicKey{}
	}
	return pub
}

func certNamesOf(leaf *x509.Certificate) []string {
	names := slices.Clone(leaf.DNSNames)
	for _, ip := range leaf.IPAddresses {
		names = append(names, ip.String())
	}
	slices.Sort(names)
	return names
}

func TestNodeCertIsIssuedByTheClusterKey(t *testing.T) {
	t.Parallel()
	test := newNodeCertTest(t)
	key := testClusterKey(t)
	test.keys.set(key)
	if err := test.cert.refresh(standardConfig()); err != nil {
		t.Fatal(err)
	}
	leaf := test.served(t)
	if !signedBy(leaf, key) {
		t.Error("the certificate is not signed by the cluster key")
	}
	if pub, ok := leaf.PublicKey.(*ecdsa.PublicKey); !ok || pub.Curve != elliptic.P256() {
		t.Errorf("public key is %T, want ECDSA P-256", leaf.PublicKey)
	}
	if !strings.Contains(leaf.Issuer.CommonName, key.ID) {
		t.Errorf("issuer = %q, want the key id %s", leaf.Issuer.CommonName, key.ID)
	}
}

func TestNodeCertHasTheNamesAndTheValidity(t *testing.T) {
	t.Parallel()
	test := newNodeCertTest(t)
	test.keys.set(testClusterKey(t))
	if err := test.cert.refresh(standardConfig()); err != nil {
		t.Fatal(err)
	}
	leaf := test.served(t)
	want := []string{"192.0.2.7", "east", "east.example.com", "test-host"}
	if got := certNamesOf(leaf); !slices.Equal(got, want) {
		t.Errorf("names = %v, want %v", got, want)
	}
	// An hour before the issue time, so that a client whose clock is a little behind accepts a
	// fresh certificate, and 90 days from the issue time.
	if !leaf.NotBefore.Equal(certTime().Add(-time.Hour)) || !leaf.NotAfter.Equal(certTime().Add(90*24*time.Hour)) {
		t.Errorf("valid %v to %v, want an hour before %v to 90 days after it", leaf.NotBefore, leaf.NotAfter, certTime())
	}
	if len(leaf.ExtKeyUsage) != 1 || leaf.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth {
		t.Errorf("extended key usage = %v", leaf.ExtKeyUsage)
	}
}

func TestNodeCertIsSavedWithThePrivateKeyKept(t *testing.T) {
	t.Parallel()
	test := newNodeCertTest(t)
	test.keys.set(testClusterKey(t))
	if err := test.cert.refresh(standardConfig()); err != nil {
		t.Fatal(err)
	}
	dir := test.cert.dir
	saved := readPEM(t, filepath.Join(dir, certFileName)).Bytes
	if !bytes.Equal(saved, test.served(t).Raw) {
		t.Error("cert.pem is not the certificate that is served")
	}
	keyBefore, err := os.ReadFile(filepath.Join(dir, keyFileName))
	if err != nil {
		t.Fatal(err)
	}
	if got := mode(t, filepath.Join(dir, keyFileName)); got != 0o600 {
		t.Errorf("key.pem has mode %o, want 600", got)
	}
	// A new cluster key makes a new certificate, still for the same node key.
	test.keys.set(testClusterKey(t))
	if err = test.cert.refresh(standardConfig()); err != nil {
		t.Fatal(err)
	}
	keyAfter, err := os.ReadFile(filepath.Join(dir, keyFileName))
	if err != nil || !bytes.Equal(keyBefore, keyAfter) {
		t.Errorf("the node key changed (read error %v)", err)
	}
}

func TestNodeCertIsReissuedWhenItIsNeeded(t *testing.T) {
	t.Parallel()
	const day = 24 * time.Hour
	tests := []struct {
		name   string
		change func(test *nodeCertTest) *config.Config
		issued int // how many certificates exist after the change, counting the first
	}{
		{"nothing changed", func(*nodeCertTest) *config.Config { return standardConfig() }, 1},
		{"31 days left", func(n *nodeCertTest) *config.Config {
			n.now = certTime().Add(59 * day)
			return standardConfig()
		}, 1},
		{"30 days left", func(n *nodeCertTest) *config.Config {
			n.now = certTime().Add(60 * day)
			return standardConfig()
		}, 2},
		{"expired", func(n *nodeCertTest) *config.Config {
			n.now = certTime().Add(100 * day)
			return standardConfig()
		}, 2},
		{"cluster key changed", func(n *nodeCertTest) *config.Config {
			n.keys.set(testClusterKey(t))
			return standardConfig()
		}, 2},
		{"url host changed", func(*nodeCertTest) *config.Config {
			return listenOn("https://west.example.com/api", "east", "192.0.2.7")
		}, 2},
		{"node name changed", func(*nodeCertTest) *config.Config {
			return listenOn("https://east.example.com/api", "north", "192.0.2.7")
		}, 2},
		{"listen address changed", func(*nodeCertTest) *config.Config {
			return listenOn("https://east.example.com/api", "east", "192.0.2.8")
		}, 2},
		{"host name changed", func(n *nodeCertTest) *config.Config {
			n.host = "renamed"
			return standardConfig()
		}, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			checkReissue(t, tt.change, tt.issued)
		})
	}
}

// checkReissue makes a certificate, applies change, and fails the test unless the certificate
// was made again exactly when issued, the number of certificates made in all, says so.
func checkReissue(t *testing.T, change func(*nodeCertTest) *config.Config, issued int) {
	t.Helper()
	test := newNodeCertTest(t)
	test.keys.set(testClusterKey(t))
	if err := test.cert.refresh(standardConfig()); err != nil {
		t.Fatal(err)
	}
	first := test.served(t)
	if err := test.cert.refresh(change(test)); err != nil {
		t.Fatal(err)
	}
	again := !bytes.Equal(first.Raw, test.served(t).Raw)
	if again != (issued > 1) {
		t.Errorf("reissued = %v, want %v", again, issued > 1)
	}
	if got := strings.Count(test.logs.String(), "node certificate issued"); got != issued {
		t.Errorf("the log says %d times that a certificate was issued\n%s", got, test.logs)
	}
}

// The daily check is the renewal of the last configuration, so it needs no configuration.
func TestNodeCertRenewUsesTheLastConfiguration(t *testing.T) {
	t.Parallel()
	test := newNodeCertTest(t)
	key := testClusterKey(t)
	test.keys.set(key)
	if err := test.cert.refresh(standardConfig()); err != nil {
		t.Fatal(err)
	}
	first := test.served(t)
	test.now = certTime().Add(61 * 24 * time.Hour)
	if err := test.cert.renew(); err != nil {
		t.Fatal(err)
	}
	renewed := test.served(t)
	if bytes.Equal(first.Raw, renewed.Raw) || !signedBy(renewed, key) ||
		!slices.Equal(certNamesOf(first), certNamesOf(renewed)) {
		t.Error("the check did not renew the certificate for the same names")
	}
}

func TestNodeCertWithoutClusterKeyIsSelfSigned(t *testing.T) {
	t.Parallel()
	test := newNodeCertTest(t)
	if err := test.cert.refresh(standardConfig()); err != nil {
		t.Fatal(err)
	}
	leaf := test.served(t)
	if leaf.CheckSignature(leaf.SignatureAlgorithm, leaf.RawTBSCertificate, leaf.Signature) != nil {
		t.Error("the certificate is not self-signed")
	}
	if !leaf.NotAfter.Equal(certTime().AddDate(10, 0, 0)) {
		t.Errorf("valid until %v, want 10 years as before", leaf.NotAfter)
	}
	// The check of a day later changes nothing.
	test.now = certTime().Add(24 * time.Hour)
	if err := test.cert.renew(); err != nil || !bytes.Equal(leaf.Raw, test.served(t).Raw) {
		t.Errorf("the self-signed certificate changed (error %v)", err)
	}
}

func TestNodeCertFollowsTheClusterKeyComingAndGoing(t *testing.T) {
	t.Parallel()
	test := newNodeCertTest(t)
	if err := test.cert.refresh(standardConfig()); err != nil {
		t.Fatal(err)
	}
	selfSignedCert := test.served(t)
	key := testClusterKey(t)
	test.keys.set(key)
	if err := test.cert.renew(); err != nil {
		t.Fatal(err)
	}
	issued := test.served(t)
	sameNodeKey := publicKeyOf(issued).Equal(publicKeyOf(selfSignedCert))
	if !signedBy(issued, key) || !sameNodeKey {
		t.Error("the cluster key did not replace the self-signed certificate under the same node key")
	}
	// A cluster key that goes leaves what is served as it is, until a key is there again.
	test.keys.clear()
	if err := test.cert.renew(); err != nil || !bytes.Equal(issued.Raw, test.served(t).Raw) {
		t.Errorf("the certificate changed when the key went (error %v)", err)
	}
}

func TestNodeCertOfListenersWithTheirOwnFilesIsNotMade(t *testing.T) {
	t.Parallel()
	test := newNodeCertTest(t)
	test.keys.set(testClusterKey(t))
	cfg := listenOn("https://east.example.com/api", "east")
	cfg.Listen = []config.Endpoint{
		configEndpoint(nil, true),
		configEndpoint(&config.TLS{CertFile: "c.pem", KeyFile: "k.pem"}, false),
	}
	if err := test.cert.refresh(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(test.cert.dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a certificate folder was made: %v", err)
	}
	if _, err := test.cert.get(nil); err == nil {
		t.Error("a certificate was served that nobody needs")
	}
	if err := test.cert.renew(); err != nil {
		t.Errorf("the check of a node that needs none failed: %v", err)
	}
}

func TestTLSConfigForReadsTheCurrentCertificate(t *testing.T) {
	t.Parallel()
	test := newNodeCertTest(t)
	test.keys.set(testClusterKey(t))
	if err := test.cert.refresh(standardConfig()); err != nil {
		t.Fatal(err)
	}
	cfg, err := tlsConfigFor(configEndpoint(nil, false), test.cert)
	if err != nil {
		t.Fatal(err)
	}
	before, err := cfg.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatal(err)
	}
	test.now = certTime().Add(61 * 24 * time.Hour)
	if err = test.cert.renew(); err != nil {
		t.Fatal(err)
	}
	after, err := cfg.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil || bytes.Equal(before.Certificate[0], after.Certificate[0]) {
		t.Errorf("the same configuration did not serve the renewed certificate (error %v)", err)
	}
}

func TestTLSConfigForFilesLoadsThem(t *testing.T) {
	t.Parallel()
	files := selfSigned(t)
	cfg, err := tlsConfigFor(configEndpoint(&config.TLS{CertFile: files.certFile, KeyFile: files.keyFile}, false), nil)
	if err != nil || len(cfg.Certificates) != 1 || cfg.GetCertificate != nil {
		t.Errorf("config = %+v, error %v", cfg, err)
	}
	missing := configEndpoint(&config.TLS{CertFile: "missing", KeyFile: "missing"}, false)
	if _, err = tlsConfigFor(missing, nil); err == nil {
		t.Error("missing files were accepted")
	}
}

func TestCertNames(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cfg  *config.Config
		host string
		want []string
	}{
		{"all sources", standardConfig(), "test-host", []string{"192.0.2.7", "east", "east.example.com", "test-host"}},
		{
			"duplicates once", listenOn("https://east/api", "east", "192.0.2.7", "192.0.2.7"), "east",
			[]string{"192.0.2.7", "east"},
		},
		{
			"url host is an address", listenOn("https://198.51.100.4:8443/api", "east"), "h",
			[]string{"198.51.100.4", "east", "h"},
		},
		{"url that cannot be read", listenOn("://bad", "east"), "h", []string{"east", "h"}},
		{"no url", listenOn("", "east"), "h", []string{"east", "h"}},
		{"names that cannot be in a certificate", listenOn("", "two words"), "höst", nil},
		{"unspecified address is no name", listenOn("", "east", "0.0.0.0", "::"), "h", []string{"east", "h"}},
		{"ipv6", listenOn("", "east", "2001:db8::1"), "h", []string{"2001:db8::1", "east", "h"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := certNamesFor(tt.cfg, tt.host); !slices.Equal(got, tt.want) {
				t.Errorf("names = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNodeCertListenerOnlyOfGeneratedCertificateIsNamed(t *testing.T) {
	t.Parallel()
	cfg := listenOn("", "east", "192.0.2.7")
	own := configEndpoint(&config.TLS{CertFile: "c", KeyFile: "k"}, false)
	own.Address = netip.MustParseAddr("192.0.2.99")
	cfg.Listen = append(cfg.Listen, own)
	if got := certNamesFor(cfg, "h"); !slices.Equal(got, []string{"192.0.2.7", "east", "h"}) {
		t.Errorf("names = %v", got)
	}
}

func TestNodeCertReissuesWhatItCannotUse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup func(t *testing.T, dir string)
	}{
		{"cert.pem is not PEM", func(t *testing.T, dir string) { t.Helper(); write(t, dir, certFileName, "garbage") }},
		{"cert.pem is not a certificate", func(t *testing.T, dir string) {
			t.Helper()
			write(t, dir, certFileName, "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n")
		}},
		{"cert.pem is for another key", func(t *testing.T, dir string) {
			t.Helper()
			other := filepath.Join(t.TempDir(), "other")
			if _, err := loadOrCreateCert(other, fixedSource()); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(other, certFileName))
			if err != nil {
				t.Fatal(err)
			}
			write(t, dir, certFileName, string(data))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			test := newNodeCertTest(t)
			key := testClusterKey(t)
			test.keys.set(key)
			if err := test.cert.refresh(standardConfig()); err != nil {
				t.Fatal(err)
			}
			tt.setup(t, test.cert.dir)
			if err := test.cert.refresh(standardConfig()); err != nil {
				t.Fatal(err)
			}
			if !signedBy(test.served(t), key) {
				t.Error("no certificate of the cluster key was made")
			}
		})
	}
}

func TestNodeCertErrors(t *testing.T) {
	t.Parallel()
	failing := errors.New("injected")
	tests := []struct {
		name  string
		setup func(t *testing.T, test *nodeCertTest)
		want  string
	}{
		{"no host name", func(_ *testing.T, n *nodeCertTest) {
			n.cert.src.hostname = hostnameOf("", failing)
		}, "host name: injected"},
		{"no randomness", func(_ *testing.T, n *nodeCertTest) {
			n.cert.src.random = iotest.ErrReader(failing)
		}, "injected"},
		{"node key is not a key", func(t *testing.T, n *nodeCertTest) {
			t.Helper()
			write(t, n.cert.dir, keyFileName, "not a key")
		}, "key.pem"},
		{"node key is not an EC key", func(t *testing.T, n *nodeCertTest) {
			t.Helper()
			write(t, n.cert.dir, keyFileName, "-----BEGIN EC PRIVATE KEY-----\nAAAA\n-----END EC PRIVATE KEY-----\n")
		}, "key.pem"},
		{"node key cannot be read", func(t *testing.T, n *nodeCertTest) {
			t.Helper()
			// A folder where the file belongs: not missing, and unreadable for every user.
			if err := os.MkdirAll(filepath.Join(n.cert.dir, keyFileName), 0o700); err != nil {
				t.Fatal(err)
			}
		}, "node key"},
		{"folder cannot be read", func(t *testing.T, n *nodeCertTest) {
			t.Helper()
			write(t, filepath.Dir(n.cert.dir), tlsDirName, "a file")
		}, "not a directory"},
		{"folder cannot be made", func(t *testing.T, n *nodeCertTest) {
			t.Helper()
			// A link to nothing where the folder belongs: its files read as missing, and the
			// folder cannot be made over it, for every user.
			symlinkToNothing(t, n.cert.dir)
		}, "file exists"},
		{"node key cannot be written", func(t *testing.T, n *nodeCertTest) {
			t.Helper()
			symlinkToNothing(t, filepath.Join(n.cert.dir, keyFileName))
		}, "file exists"},
		{"the time cannot be in a certificate", func(_ *testing.T, n *nodeCertTest) {
			n.now = time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC)
		}, "issue node certificate"},
		{"certificate cannot be written", func(t *testing.T, n *nodeCertTest) {
			t.Helper()
			// A folder where the file belongs: the rename over it fails for every user.
			if err := os.MkdirAll(filepath.Join(n.cert.dir, certFileName, "inside"), 0o700); err != nil {
				t.Fatal(err)
			}
		}, "file exists"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			test := newNodeCertTest(t)
			test.keys.set(testClusterKey(t))
			tt.setup(t, test)
			err := test.cert.refresh(standardConfig())
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want one containing %q", err, tt.want)
			}
			if _, getErr := test.cert.get(nil); getErr == nil {
				t.Error("a certificate is served after the failure")
			}
		})
	}
}

func TestNodeCertSelfSignedFailureIsReported(t *testing.T) {
	t.Parallel()
	test := newNodeCertTest(t)
	test.cert.src.hostname = hostnameOf("", errors.New("injected"))
	if err := test.cert.refresh(standardConfig()); err == nil || !strings.Contains(err.Error(), "injected") {
		t.Errorf("err = %v", err)
	}
}

func TestNodeCertSerialsDiffer(t *testing.T) {
	t.Parallel()
	test := newNodeCertTest(t)
	test.keys.set(testClusterKey(t))
	if err := test.cert.refresh(standardConfig()); err != nil {
		t.Fatal(err)
	}
	first := test.served(t)
	test.now = certTime().Add(61 * 24 * time.Hour)
	if err := test.cert.renew(); err != nil {
		t.Fatal(err)
	}
	if first.SerialNumber.Cmp(test.served(t).SerialNumber) == 0 {
		t.Error("two certificates have the same serial number")
	}
}

// symlinkToNothing makes a link at path that points nowhere, with the folder above it.
func symlinkToNothing(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(filepath.Dir(path), "nowhere"), path); err != nil {
		t.Fatal(err)
	}
}

// curveWithoutName is a curve that a key cannot be written for.
type curveWithoutName struct{ elliptic.Curve }

func TestEncodeKeyRefusesAKeyOfAnUnknownCurve(t *testing.T) {
	t.Parallel()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key.Curve = curveWithoutName{key.Curve}
	if _, err = encodeKey(key); err == nil {
		t.Error("a key of an unknown curve was encoded")
	}
}

func TestNodeCertRenewReportsWhatItCannotMake(t *testing.T) {
	t.Parallel()
	test := newNodeCertTest(t)
	test.keys.set(testClusterKey(t))
	if err := test.cert.refresh(standardConfig()); err != nil {
		t.Fatal(err)
	}
	served := test.served(t)
	test.cert.src.hostname = hostnameOf("", errors.New("injected"))
	if err := test.cert.renew(); err == nil || !strings.Contains(err.Error(), "host name: injected") {
		t.Errorf("err = %v", err)
	}
	if !bytes.Equal(served.Raw, test.served(t).Raw) {
		t.Error("the served certificate changed when the check failed")
	}
}

// issuedCount is how many times the node says it issued a certificate.
func (n *nodeCertTest) issuedCount() int {
	return strings.Count(n.logs.String(), "node certificate issued")
}

// A handshake that finds a cluster key that the served certificate was not made under issues
// the certificate before it answers: no reload and no daily check is needed.
func TestNodeCertHandshakePicksUpAClusterKeyThatAppears(t *testing.T) {
	t.Parallel()
	test := newNodeCertTest(t)
	if err := test.cert.refresh(standardConfig()); err != nil {
		t.Fatal(err)
	}
	selfSignedCert := test.served(t)
	key := testClusterKey(t)
	test.keys.set(key)

	leaf := test.served(t)

	if !signedBy(leaf, key) || !publicKeyOf(leaf).Equal(publicKeyOf(selfSignedCert)) {
		t.Error("the handshake did not serve a certificate under the cluster key for the same node key")
	}
	if want := []string{"192.0.2.7", "east", "east.example.com", "test-host"}; !slices.Equal(certNamesOf(leaf), want) {
		t.Errorf("names = %v, want %v", certNamesOf(leaf), want)
	}
	if saved := readPEM(t, filepath.Join(test.cert.dir, certFileName)).Bytes; !bytes.Equal(saved, leaf.Raw) {
		t.Error("cert.pem is not the certificate that is served")
	}
	// The next handshake finds the key it has already served.
	if again := test.served(t); !bytes.Equal(again.Raw, leaf.Raw) || test.issuedCount() != 1 {
		t.Errorf("the second handshake issued again: %d issued\n%s", test.issuedCount(), test.logs)
	}
}

func TestNodeCertHandshakePicksUpAChangedClusterKey(t *testing.T) {
	t.Parallel()
	test := newNodeCertTest(t)
	test.keys.set(testClusterKey(t))
	if err := test.cert.refresh(standardConfig()); err != nil {
		t.Fatal(err)
	}
	changed := testClusterKey(t)
	test.keys.set(changed)

	if leaf := test.served(t); !signedBy(leaf, changed) || test.issuedCount() != 2 {
		t.Errorf("the handshake did not serve a certificate of the changed key: %d issued", test.issuedCount())
	}
}

// A cluster key that goes leaves what is served, as the daily check does; and when it is back
// as it was, nothing is issued.
func TestNodeCertHandshakeKeepsTheCertificateWhenTheClusterKeyGoes(t *testing.T) {
	t.Parallel()
	test := newNodeCertTest(t)
	key := testClusterKey(t)
	test.keys.set(key)
	if err := test.cert.refresh(standardConfig()); err != nil {
		t.Fatal(err)
	}
	issued := test.served(t)
	test.keys.clear()
	if kept := test.served(t); !bytes.Equal(kept.Raw, issued.Raw) {
		t.Error("the certificate changed when the key went")
	}
	test.keys.set(key)
	if back := test.served(t); !bytes.Equal(back.Raw, issued.Raw) || test.issuedCount() != 1 {
		t.Errorf("a key that came back as it was issued again: %d issued", test.issuedCount())
	}
}

// Handshakes that arrive together while the key is new make one certificate: one issues, and
// the others, which waited, get the same one. They all leave the starting gate at once.
func TestNodeCertHandshakesAtOnceIssueOnce(t *testing.T) {
	t.Parallel()
	const handshakes = 16
	test := newNodeCertTest(t)
	if err := test.cert.refresh(standardConfig()); err != nil {
		t.Fatal(err)
	}
	key := testClusterKey(t)
	test.keys.set(key)
	begin := make(chan struct{})
	served := make([][]byte, handshakes)
	var done sync.WaitGroup
	for i := range handshakes {
		done.Go(func() {
			<-begin
			cert, err := test.cert.get(nil)
			if err != nil {
				t.Error(err)
				return
			}
			served[i] = cert.Certificate[0]
		})
	}
	close(begin)
	done.Wait()

	for i, raw := range served {
		if !bytes.Equal(raw, served[0]) {
			t.Errorf("handshake %d got another certificate than handshake 0", i)
		}
	}
	if leaf := test.served(t); !signedBy(leaf, key) || test.issuedCount() != 1 {
		t.Errorf("%d certificates issued, want one under the cluster key", test.issuedCount())
	}
}

// A handshake that waited for the lock finds the certificate made by the one before it, and
// issues nothing. The state is made here directly, so that the test does not depend on which
// handshake gets the lock first.
func TestNodeCertHandshakeThatWaitedFindsTheCertificateMade(t *testing.T) {
	t.Parallel()
	test := newNodeCertTest(t)
	key := testClusterKey(t)
	test.keys.set(key)
	if err := test.cert.refresh(standardConfig()); err != nil {
		t.Fatal(err)
	}
	issued := test.served(t)

	cert, err := test.cert.pickUp(key)

	if err != nil || !bytes.Equal(cert.Certificate[0], issued.Raw) || test.issuedCount() != 1 {
		t.Errorf("pickUp = %v, %d issued, want the certificate that was made and no new one", err, test.issuedCount())
	}
}

// A handshake that cannot issue serves the certificate it has and says so; it does not try
// again at every handshake, but the daily check does.
func TestNodeCertHandshakeThatCannotIssueKeepsTheCertificate(t *testing.T) {
	t.Parallel()
	test := newNodeCertTest(t)
	if err := test.cert.refresh(standardConfig()); err != nil {
		t.Fatal(err)
	}
	selfSignedCert := test.served(t)
	attempts := 0
	test.cert.src.hostname = func() (string, error) {
		attempts++
		return "", errors.New("injected")
	}
	key := testClusterKey(t)
	test.keys.set(key)

	for range 3 {
		if kept := test.served(t); !bytes.Equal(kept.Raw, selfSignedCert.Raw) {
			t.Fatal("a certificate was served that was not the one the node had")
		}
	}

	if attempts != 1 || strings.Count(test.logs.String(), "cannot issue the node certificate") != 1 {
		t.Errorf("%d attempts, log:\n%s, want one attempt and one warning", attempts, test.logs)
	}
	test.cert.src.hostname = hostnameOf("test-host", nil)
	if err := test.cert.renew(); err != nil || !signedBy(test.served(t), key) {
		t.Errorf("the daily check did not issue under the cluster key (error %v)", err)
	}
}

// With nothing served yet, a handshake that cannot issue has nothing to fall back on.
func TestNodeCertHandshakeThatCannotIssueAndHasNothingFails(t *testing.T) {
	t.Parallel()
	test := newNodeCertTest(t)
	test.cert.wanted = wanted{needed: true, names: []string{"east"}}
	test.cert.src.hostname = hostnameOf("", errors.New("injected"))
	test.keys.set(testClusterKey(t))

	if _, err := test.cert.get(nil); err == nil || !strings.Contains(err.Error(), "injected") {
		t.Errorf("get = %v, want the error of issuing", err)
	}
}
