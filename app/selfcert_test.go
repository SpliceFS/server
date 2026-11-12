package app

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

var certTime = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

// certDir is where a test keeps its certificate: below a state directory that does not exist
// yet, because creating the folders is part of what is tested.
func certDir(t *testing.T) (state, dir string) {
	t.Helper()
	state = filepath.Join(t.TempDir(), "state")
	return state, filepath.Join(state, localDirName, tlsDirName)
}

func hostnameOf(name string, err error) func() (string, error) {
	return func() (string, error) { return name, err }
}

func fixedSource() certSource {
	src := testCertSource()
	src.now = func() time.Time { return certTime }
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

func TestLocalCertificateContent(t *testing.T) {
	t.Parallel()
	state, dir := certDir(t)
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
	if !parsed.NotBefore.Equal(certTime) || !parsed.NotAfter.Equal(certTime.AddDate(10, 0, 0)) {
		t.Errorf("valid %v to %v", parsed.NotBefore, parsed.NotAfter)
	}
	pub, ok := parsed.PublicKey.(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() {
		t.Errorf("public key is %T, want ECDSA P-256", parsed.PublicKey)
	}
	if parsed.CheckSignature(parsed.SignatureAlgorithm, parsed.RawTBSCertificate,
		parsed.Signature) != nil {
		t.Error("the certificate is not self-signed")
	}
	if len(parsed.ExtKeyUsage) != 1 || parsed.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth {
		t.Errorf("extended key usage = %v", parsed.ExtKeyUsage)
	}

	for path, want := range map[string]os.FileMode{
		filepath.Join(state, localDirName): 0o700,
		dir:                                0o700,
		filepath.Join(dir, keyFileName):    0o600,
	} {
		if got := mode(t, path); got != want {
			t.Errorf("%s has mode %o, want %o", path, got, want)
		}
	}
}

func TestLocalCertificateIsNeverRegenerated(t *testing.T) {
	t.Parallel()
	_, dir := certDir(t)
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

func TestLocalCertificateErrors(t *testing.T) {
	t.Parallel()
	failing := errors.New("injected")
	tests := []struct {
		name   string
		setup  func(t *testing.T, dir string)
		mutate func(src *certSource)
		want   string
		// absent lists files that a failure must not leave behind.
		absent []string
	}{
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
			name:   "host name that cannot be in a certificate",
			mutate: func(src *certSource) { src.hostname = hostnameOf("h\u00f6st", nil) },
			want:   "IA5String",
			absent: []string{keyFileName, certFileName},
		},
		{
			name: "folder cannot be created",
			setup: func(t *testing.T, dir string) {
				// Nothing can be created inside a folder that is read-only.
				if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(filepath.Dir(dir), 0o500); err != nil {
					t.Fatal(err)
				}
			},
			want: "permission denied",
		},
		{
			name: "key exists without a certificate",
			setup: func(t *testing.T, dir string) {
				write(t, dir, keyFileName, "operator key")
			},
			want: "file exists",
		},
		{
			name: "certificate cannot be written",
			setup: func(t *testing.T, dir string) {
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
				write(t, dir, certFileName, "not a certificate")
				write(t, dir, keyFileName, "not a key")
			},
			want: "failed to find any PEM data",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, dir := certDir(t)
			if tt.setup != nil {
				tt.setup(t, dir)
			}
			src := fixedSource()
			if tt.mutate != nil {
				tt.mutate(&src)
			}
			_, err := loadOrCreateCert(dir, src)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want one containing %q", err, tt.want)
			}
			for _, name := range tt.absent {
				if _, err := os.Lstat(filepath.Join(dir, name)); err == nil {
					t.Errorf("%s was left behind", name)
				}
			}
		})
	}
}

func TestLocalCertificateKeepsFilesTheOperatorPlaced(t *testing.T) {
	t.Parallel()
	_, dir := certDir(t)
	key := write(t, dir, keyFileName, "operator key")
	if _, err := loadOrCreateCert(dir, fixedSource()); err == nil {
		t.Fatal("a certificate was made next to an existing key")
	}
	if data, err := os.ReadFile(key); err != nil || string(data) != "operator key" {
		t.Errorf("the key was overwritten: %q, %v", data, err)
	}
}
