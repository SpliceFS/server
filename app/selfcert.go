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
	// serialBits is the size of a certificate serial number. It is below 128 bits so that the
	// number stays positive in a signed 128 bit field, and far above what makes a clash likely.
	serialBits = 127
	dirMode    = 0o700
	keyMode    = 0o600
	certMode   = 0o644
)

// loopbackAddress is the address that every certificate of the server names.
const loopbackAddress = "127.0.0.1"

// certSource is what making a certificate takes from the outside world.
type certSource struct {
	now      func() time.Time
	hostname func() (string, error)
	random   io.Reader
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
	pemFiles, err := generateCert(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return fmt.Errorf("create certificate folder: %w", err)
	}
	keyPath := filepath.Join(dir, keyFileName)
	if err := writeNew(keyPath, pemFiles.key, keyMode); err != nil {
		return err
	}
	if err := writeNew(filepath.Join(dir, certFileName), pemFiles.cert, certMode); err != nil {
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
		return fmt.Errorf("create certificate file: %w", err)
	}
	_, writeErr := f.Write(data)
	return errors.Join(writeErr, f.Close())
}

// pemPair is a certificate and its private key, both PEM encoded.
type pemPair struct {
	cert, key []byte
}

// generateCert makes a self-signed ECDSA P-256 certificate for the host name and the
// loopback names, and returns it and its key as PEM.
func generateCert(src certSource) (pemPair, error) {
	host, err := src.hostname()
	if err != nil {
		return pemPair{}, fmt.Errorf("host name: %w", err)
	}
	key, serial, err := newKeyAndSerial(src.random)
	if err != nil {
		return pemPair{}, err
	}
	return encodeCert(src.random, certTemplate(certHost(host), serial, src.now()), key)
}

// newKeyAndSerial makes a node key and the serial number of its first certificate. They are
// made together because both draw on random, which is what a failure of it is tested with.
func newKeyAndSerial(random io.Reader) (*ecdsa.PrivateKey, *big.Int, error) {
	key, keyErr := ecdsa.GenerateKey(elliptic.P256(), random)
	serial, serialErr := newSerial(random)
	if err := errors.Join(keyErr, serialErr); err != nil {
		return nil, nil, err
	}
	return key, serial, nil
}

// newSerial returns a random certificate serial number.
func newSerial(random io.Reader) (*big.Int, error) {
	serial, err := rand.Int(random, new(big.Int).Lsh(big.NewInt(1), serialBits))
	if err != nil {
		return nil, fmt.Errorf("serial number: %w", err)
	}
	return serial, nil
}

// encodeCert signs template with key and returns the certificate and the key as PEM.
func encodeCert(
	random io.Reader, template *x509.Certificate, key *ecdsa.PrivateKey,
) (pemPair, error) {
	certDER, certErr := x509.CreateCertificate(random, template, template, &key.PublicKey, key)
	keyPEM, keyErr := encodeKey(key)
	if err := errors.Join(certErr, keyErr); err != nil {
		return pemPair{}, err
	}
	return pemPair{cert: encodeDER(certDER), key: keyPEM}, nil
}

// encodeDER returns a DER certificate as PEM.
func encodeDER(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// encodeKey returns the private key as PEM.
func encodeKey(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("encode node key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), nil
}

// certHost returns host if it can be a name in a certificate, which holds only printable
// ASCII, and "" otherwise. A server with nothing configured must start on any machine, so
// a host name that does not fit is left out and the certificate names only the loopback.
func certHost(host string) string {
	for _, r := range host {
		if r <= ' ' || r > '~' {
			return ""
		}
	}
	return host
}

// certTemplate describes a server certificate valid for certYears from now.
func certTemplate(host string, serial *big.Int, now time.Time) *x509.Certificate {
	names := []string{"localhost"}
	if host != "" {
		names = []string{host, "localhost"}
	}
	return &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: names[0]},
		NotBefore:    now,
		NotAfter:     now.AddDate(certYears, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     names,
		IPAddresses:  []net.IP{net.ParseIP(loopbackAddress), net.IPv6loopback},
	}
}
