package app

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

// certTime is the time of the clock that the certificates of the tests are made with.
func certTime() time.Time {
	return time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
}

// certDir is where a test keeps its certificate: below a state directory that does not exist
// yet, because creating the folders is part of what is tested.
func certDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "state", localDirName, tlsDirName)
}

func hostnameOf(name string, err error) func() (string, error) {
	return func() (string, error) { return name, err }
}

func fixedSource() certSource {
	src := testCertSource()
	src.now = certTime
	return src
}

func readPEM(t *testing.T, path string) *pem.Block {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		t.Fatalf("%s holds no PEM block", path)
	}
	return block
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

// localTestCert makes the certificate of a new server and returns it as it was saved.
func localTestCert(t *testing.T) *x509.Certificate {
	t.Helper()
	dir := certDir(t)
	cert, err := loadOrCreateCert(dir, fixedSource())
	if err != nil {
		t.Fatal(err)
	}
	if len(cert.Certificate) != 1 {
		t.Fatalf("certificate chain has %d entries", len(cert.Certificate))
	}
	parsed, err := x509.ParseCertificate(readPEM(t, filepath.Join(dir, certFileName)).Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func TestLocalCertificateNames(t *testing.T) {
	t.Parallel()
	parsed := localTestCert(t)
	if parsed.Subject.CommonName != "test-host" {
		t.Errorf("subject = %q", parsed.Subject.CommonName)
	}
	if want := []string{"test-host", "localhost"}; !slices.Equal(parsed.DNSNames, want) {
		t.Errorf("DNS names = %v, want %v", parsed.DNSNames, want)
	}
	if len(parsed.IPAddresses) != 2 || !parsed.IPAddresses[0].Equal(net.IPv4(127, 0, 0, 1)) ||
		!parsed.IPAddresses[1].Equal(net.IPv6loopback) {
		t.Errorf("IP addresses = %v", parsed.IPAddresses)
	}
}

func TestLocalCertificateValidity(t *testing.T) {
	t.Parallel()
	parsed := localTestCert(t)
	if !parsed.NotBefore.Equal(certTime()) || !parsed.NotAfter.Equal(certTime().AddDate(10, 0, 0)) {
		t.Errorf("valid %v to %v", parsed.NotBefore, parsed.NotAfter)
	}
}

func TestLocalCertificateKey(t *testing.T) {
	t.Parallel()
	parsed := localTestCert(t)
	pub, ok := parsed.PublicKey.(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() {
		t.Errorf("public key is %T, want ECDSA P-256", parsed.PublicKey)
	}
	if parsed.CheckSignature(parsed.SignatureAlgorithm, parsed.RawTBSCertificate,
		parsed.Signature) != nil {
		t.Error("the certificate is not self-signed")
	}
}

func TestLocalCertificateIsForServers(t *testing.T) {
	t.Parallel()
	parsed := localTestCert(t)
	if len(parsed.ExtKeyUsage) != 1 || parsed.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth {
		t.Errorf("extended key usage = %v", parsed.ExtKeyUsage)
	}
}

func TestLocalCertificateFilesArePrivate(t *testing.T) {
	t.Parallel()
	dir := certDir(t)
	if _, err := loadOrCreateCert(dir, fixedSource()); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{
		filepath.Dir(dir):               0o700,
		dir:                             0o700,
		filepath.Join(dir, keyFileName): 0o600,
	} {
		if got := mode(t, path); got != want {
			t.Errorf("%s has mode %o, want %o", path, got, want)
		}
	}
}

func TestLocalCertificateIsNeverRegenerated(t *testing.T) {
	t.Parallel()
	dir := certDir(t)
	first, err := loadOrCreateCert(dir, fixedSource())
	if err != nil {
		t.Fatal(err)
	}
	certBefore, err := os.ReadFile(filepath.Join(dir, certFileName))
	if err != nil {
		t.Fatal(err)
	}
	// A second start that could not make a certificate still succeeds, which proves that
	// it loaded the files.
	broken := certSource{
		now:      func() time.Time { panic("the clock was read") },
		hostname: func() (string, error) { return "", errors.New("no host name") },
		random:   iotest.ErrReader(errors.New("no randomness")),
	}
	second, err := loadOrCreateCert(dir, broken)
	if err != nil {
		t.Fatal(err)
	}
	certAfter, err := os.ReadFile(filepath.Join(dir, certFileName))
	sameFile := bytes.Equal(certBefore, certAfter)
	if err != nil || !sameFile || !bytes.Equal(first.Certificate[0], second.Certificate[0]) {
		t.Errorf("the certificate changed (read error %v)", err)
	}
}

func TestLocalCertificateUsesTheStateDirectory(t *testing.T) {
	t.Parallel()
	state := t.TempDir()
	if _, err := localCertificate(state); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{certFileName, keyFileName} {
		if _, err := os.Stat(filepath.Join(state, "local", "tls", name)); err != nil {
			t.Error(err)
		}
	}
}

// certFailure is a way to make the creation of the certificate fail.
type certFailure struct {
	name   string
	setup  func(t *testing.T, dir string) // prepares the folder of the certificate
	mutate func(src *certSource)          // breaks one of the sources of the certificate
	want   string                         // part of the expected error
	// absent lists files that a failure must not leave behind.
	absent []string
}

// run makes a certificate in a new folder and fails the test unless that fails the way
// the case says.
func (c certFailure) run(t *testing.T) {
	t.Helper()
	dir := certDir(t)
	if c.setup != nil {
		c.setup(t, dir)
	}
	src := fixedSource()
	if c.mutate != nil {
		c.mutate(&src)
	}
	_, err := loadOrCreateCert(dir, src)
	if err == nil || !strings.Contains(err.Error(), c.want) {
		t.Fatalf("err = %v, want one containing %q", err, c.want)
	}
	for _, name := range c.absent {
		if _, err = os.Lstat(filepath.Join(dir, name)); err == nil {
			t.Errorf("%s was left behind", name)
		}
	}
}

func TestLocalCertificateErrors(t *testing.T) {
	t.Parallel()
	failing := errors.New("injected")
	tests := []certFailure{
		{
			name:   "no host name",
			mutate: func(src *certSource) { src.hostname = hostnameOf("", failing) },
			want:   "host name: injected",
			absent: []string{keyFileName, certFileName},
		},
		{
			name:   "no randomness",
			mutate: func(src *certSource) { src.random = iotest.ErrReader(failing) },
			want:   "injected",
			absent: []string{keyFileName, certFileName},
		},
		{
			name: "folder cannot be created",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				// A link to nothing where the folder belongs: its files read as missing, and
				// the folder cannot be made over it. This stops every user, root included,
				// which a read-only folder does not.
				if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(filepath.Dir(dir), "nowhere"), dir); err != nil {
					t.Fatal(err)
				}
			},
			want: "file exists",
		},
		{
			name: "key exists without a certificate",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				write(t, dir, keyFileName, "operator key")
			},
			want: "file exists",
		},
		{
			name: "certificate cannot be written",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				// A link to nothing reads as missing but cannot be created over.
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				link := filepath.Join(dir, certFileName)
				if err := os.Symlink(filepath.Join(dir, "nowhere"), link); err != nil {
					t.Fatal(err)
				}
			},
			want:   "file exists",
			absent: []string{keyFileName},
		},
		{
			name: "existing files are unusable",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				write(t, dir, certFileName, "not a certificate")
				write(t, dir, keyFileName, "not a key")
			},
			want: "failed to find any PEM data",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.run(t)
		})
	}
}

func TestLocalCertificateKeepsFilesTheOperatorPlaced(t *testing.T) {
	t.Parallel()
	dir := certDir(t)
	key := write(t, dir, keyFileName, "operator key")
	if _, err := loadOrCreateCert(dir, fixedSource()); err == nil {
		t.Fatal("a certificate was made next to an existing key")
	}
	if data, err := os.ReadFile(key); err != nil || string(data) != "operator key" {
		t.Errorf("the key was overwritten: %q, %v", data, err)
	}
}

// A host name that cannot be a name in a certificate must not stop the server: the
// certificate is made without it.
func TestCertificateWithoutUsableHostName(t *testing.T) {
	t.Parallel()
	for _, host := range []string{"h\u00f6st", "two words", ""} {
		dir := certDir(t)
		src := fixedSource()
		src.hostname = hostnameOf(host, nil)
		cert, err := loadOrCreateCert(dir, src)
		if err != nil {
			t.Fatalf("host %q: %v", host, err)
		}
		leaf, err := x509.ParseCertificate(cert.Certificate[0])
		if err != nil {
			t.Fatal(err)
		}
		if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != "localhost" {
			t.Errorf("host %q: names = %v, want only localhost", host, leaf.DNSNames)
		}
	}
}

func TestEncodeCertRefusesABadTemplate(t *testing.T) {
	t.Parallel()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := certTemplate("h\u00f6st", big.NewInt(1), time.Now())
	if _, err := encodeCert(rand.Reader, template, key); err == nil {
		t.Error("a name that cannot be encoded was accepted")
	}
}
