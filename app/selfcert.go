package app

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// Where the certificate that the server makes for itself lives, below the state directory.
// The local folder is never synced between servers, because the private key must not leave
// this machine.
const (
	localDirName = "local"
	tlsDirName   = "tls"
	certFileName = "cert.pem"
	keyFileName  = "key.pem"

	certYears = 10
	dirMode   = 0o700
	keyMode   = 0o600
	certMode  = 0o644
)

// certSource is what making a certificate takes from the outside world.
type certSource struct {
	now      func() time.Time
	hostname func() (string, error)
	random   io.Reader
}

// localCertificate returns the certificate of this server, making it on first use.
func localCertificate(stateDir string) (tls.Certificate, error) {
	dir := filepath.Join(stateDir, localDirName, tlsDirName)
	src := certSource{now: time.Now, hostname: os.Hostname, random: rand.Reader}
	return loadOrCreateCert(dir, src)
}

// loadOrCreateCert loads cert.pem and key.pem from dir. Only when neither can be found are
// they made; files that exist are never replaced, even when they are unusable, because the
// operator may have put them there on purpose.
func loadOrCreateCert(dir string, src certSource) (tls.Certificate, error) {
	certPath, keyPath := filepath.Join(dir, certFileName), filepath.Join(dir, keyFileName)
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if errors.Is(err, fs.ErrNotExist) {
		if err = createCert(dir, src); err == nil {
			cert, err = tls.LoadX509KeyPair(certPath, keyPath)
		}
	}
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("self-signed certificate in %s: %w", dir, err)
	}
	return cert, nil
}

// createCert writes a new key and certificate into dir, which it creates if needed. The
// files are created exclusively so that nothing is overwritten, and the key goes first so
// that a failure never leaves a certificate without its key.
func createCert(dir string, src certSource) error {
	certPEM, keyPEM, err := generateCert(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return err
	}
	keyPath := filepath.Join(dir, keyFileName)
	if err := writeNew(keyPath, keyPEM, keyMode); err != nil {
		return err
	}
	if err := writeNew(filepath.Join(dir, certFileName), certPEM, certMode); err != nil {
		// A key alone would make every later start fail instead of trying again.
		return errors.Join(err, os.Remove(keyPath))
	}
	return nil
}

// writeNew creates the file at path with mode and writes data to it. It fails if the file
// exists.
func writeNew(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(data)
	return errors.Join(writeErr, f.Close())
}

// generateCert makes a self-signed ECDSA P-256 certificate for the host name and the
// loopback names, and returns it and its key as PEM.
func generateCert(src certSource) (certPEM, keyPEM []byte, err error) {
	host, err := src.hostname()
	if err != nil {
		return nil, nil, fmt.Errorf("host name: %w", err)
	}
	key, keyErr := ecdsa.GenerateKey(elliptic.P256(), src.random)
	serial, serialErr := rand.Int(src.random, new(big.Int).Lsh(big.NewInt(1), 127))
	if err := errors.Join(keyErr, serialErr); err != nil {
		return nil, nil, err
	}
	template := certTemplate(host, serial, src.now())
	certDER, certErr := x509.CreateCertificate(src.random, template, template, &key.PublicKey, key)
	keyDER, keyDERErr := x509.MarshalECPrivateKey(key)
	if err := errors.Join(certErr, keyDERErr); err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), nil
}

// certTemplate describes a server certificate valid for certYears from now.
func certTemplate(host string, serial *big.Int, now time.Time) *x509.Certificate {
	return &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    now,
		NotAfter:     now.AddDate(certYears, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{host, "localhost"},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
}
