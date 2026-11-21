package app

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"time"

	"github.com/SpliceFS/server/cluster"
	"github.com/SpliceFS/server/config"
	"github.com/SpliceFS/server/internal/datadir"
)

// How long a certificate that the cluster key signs lives, and how long before its end it is
// made again. A server that runs is checked once a day, so a renewal has 30 days to succeed.
const (
	nodeCertLifetime    = 90 * 24 * time.Hour
	nodeCertRenewBefore = 30 * 24 * time.Hour
)

var (
	errNodeKeyPEM = errors.New("node key in key.pem must be an EC private key in PEM form")
	errNoNodeCert = errors.New("this node has no certificate, because no listener uses the generated one")
)

// nodeCert is the certificate of the listeners that name no certificate files. With a cluster
// key it is issued by that key for the names of this node, to the node key in key.pem, and
// made again when it is about to end or the key or the names change. Without one it is the
// self-signed certificate. Only the goroutine that serves calls refresh and renew; get is
// called by every handshake.
type nodeCert struct {
	dir    string
	src    certSource
	key    func() (cluster.Key, bool)
	logger *slog.Logger
	// wanted is what the last configuration asks of the certificate. The daily check makes
	// the certificate for it again, so it needs no configuration.
	wanted  wanted
	current atomic.Pointer[tls.Certificate]
}

// wanted is what a configuration says about the certificate: whether any listener uses it,
// and the names it is made of, which the host name completes when the certificate is made.
type wanted struct {
	needed bool
	names  []string
}

// newNodeCert returns the certificate of this node, kept in dir. The cluster key is read
// through key each time the certificate is checked.
func newNodeCert(dir string, src certSource, key func() (cluster.Key, bool), logger *slog.Logger) *nodeCert {
	return &nodeCert{dir: dir, src: src, key: key, logger: logger}
}

// get is the certificate for a new connection, so a certificate that was made since the last
// connection is served at once.
func (n *nodeCert) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	if cert := n.current.Load(); cert != nil {
		return cert, nil
	}
	return nil, errNoNodeCert
}

// refresh makes the certificate right for cfg. A configuration without a listener that uses
// the generated certificate needs none, and makes none.
func (n *nodeCert) refresh(cfg *config.Config) error {
	n.wanted = wanted{
		needed: slices.ContainsFunc(cfg.Listen, usesGeneratedCert),
		names:  certNamesFor(cfg, ""),
	}
	return n.renew()
}

// renew makes the certificate right for the last configuration. It changes nothing when the
// certificate already is.
func (n *nodeCert) renew() error {
	if !n.wanted.needed {
		return nil
	}
	clusterKey, ok := n.key()
	if !ok {
		return n.selfSigned()
	}
	return n.issueUnder(clusterKey)
}

// selfSigned serves the self-signed certificate, which it makes only when the node has none.
// A node that has served a certificate keeps it when the cluster key goes away.
func (n *nodeCert) selfSigned() error {
	if n.current.Load() != nil {
		return nil
	}
	cert, err := loadOrCreateCert(n.dir, n.src)
	if err != nil {
		return err
	}
	n.current.Store(&cert)
	return nil
}

// issueUnder serves the saved certificate when it is still right, and otherwise issues one
// under clusterKey, for the same node key.
func (n *nodeCert) issueUnder(clusterKey cluster.Key) error {
	host, err := n.src.hostname()
	if err != nil {
		return fmt.Errorf("host name: %w", err)
	}
	names := joinNames(n.wanted.names, []string{host})
	// The new key is the node key only when key.pem has none; making it here keeps the
	// randomness of the certificate in one step.
	fresh, serial, err := newKeyAndSerial(n.src.random)
	if err != nil {
		return err
	}
	nodeKey, err := n.nodeKey(fresh)
	if err != nil {
		return err
	}
	now := n.src.now()
	if leaf, ok := n.saved(nodeKey); ok && stillRight(leaf, clusterKey, names, now) {
		n.serve(leaf.Raw, nodeKey)
		return nil
	}
	return n.issue(clusterKey, nodeKey, nodeTemplate(names, serial, now))
}

// issue signs template for nodeKey with the cluster key, saves the certificate and serves it.
func (n *nodeCert) issue(clusterKey cluster.Key, nodeKey *ecdsa.PrivateKey, template *x509.Certificate) error {
	der, err := x509.CreateCertificate(n.src.random, template, clusterIssuer(clusterKey),
		&nodeKey.PublicKey, clusterKey.Private)
	if err != nil {
		return fmt.Errorf("issue node certificate: %w", err)
	}
	if err = datadir.WriteAtomic(filepath.Join(n.dir, certFileName), encodeDER(der), certMode); err != nil {
		return fmt.Errorf("save node certificate: %w", err)
	}
	n.serve(der, nodeKey)
	n.logger.Info("node certificate issued", slog.Time("expires", template.NotAfter))
	return nil
}

// serve makes the certificate der the one of new connections.
func (n *nodeCert) serve(der []byte, nodeKey *ecdsa.PrivateKey) {
	n.current.Store(&tls.Certificate{Certificate: [][]byte{der}, PrivateKey: nodeKey})
}

// nodeKey returns the node key of key.pem. When there is no such file it saves fresh as the
// node key and returns it. A file that is there but cannot be used is an error and is never
// replaced: the operator may have put it there on purpose.
func (n *nodeCert) nodeKey(fresh *ecdsa.PrivateKey) (*ecdsa.PrivateKey, error) {
	path := filepath.Join(n.dir, keyFileName)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return fresh, saveNodeKey(n.dir, path, fresh)
	}
	if err != nil {
		return nil, fmt.Errorf("node key: %w", err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("%s: %w", path, errNodeKeyPEM)
	}
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, errNodeKeyPEM)
	}
	return key, nil
}

// saveNodeKey saves key at path, in the folder dir that it makes if needed.
func saveNodeKey(dir, path string, key *ecdsa.PrivateKey) error {
	keyPEM, pemErr := encodeKey(key)
	mkdirErr := os.MkdirAll(dir, dirMode)
	if err := errors.Join(pemErr, mkdirErr); err != nil {
		return fmt.Errorf("create node key: %w", err)
	}
	return writeNew(path, keyPEM, keyMode)
}

// saved returns the certificate in cert.pem when it can be read and is for nodeKey.
func (n *nodeCert) saved(nodeKey *ecdsa.PrivateKey) (*x509.Certificate, bool) {
	data, err := os.ReadFile(filepath.Join(n.dir, certFileName))
	if err != nil {
		return nil, false
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, false
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, false
	}
	pub, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	return leaf, ok && pub.Equal(&nodeKey.PublicKey)
}

// stillRight reports whether leaf needs no new one: the cluster key signed it, it names
// exactly names, and it has more than the renewal margin left at now.
func stillRight(leaf *x509.Certificate, clusterKey cluster.Key, names []string, now time.Time) bool {
	public := ed25519.PublicKey(clusterKey.Private[ed25519.SeedSize:])
	return leaf.SignatureAlgorithm == x509.PureEd25519 &&
		ed25519.Verify(public, leaf.RawTBSCertificate, leaf.Signature) &&
		leaf.NotAfter.Sub(now) > nodeCertRenewBefore &&
		slices.Equal(namesOf(leaf), names)
}

// namesOf returns the names of leaf in the form certNamesFor makes them.
func namesOf(leaf *x509.Certificate) []string {
	names := slices.Clone(leaf.DNSNames)
	for _, ip := range leaf.IPAddresses {
		names = append(names, ip.String())
	}
	return joinNames(names)
}

// nodeTemplate describes the certificate of a node, valid for nodeCertLifetime from now.
func nodeTemplate(names []string, serial *big.Int, now time.Time) *x509.Certificate {
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "splicefs node"},
		NotBefore:             now,
		NotAfter:              now.Add(nodeCertLifetime),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	for _, name := range names {
		if ip := net.ParseIP(name); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
		} else {
			template.DNSNames = append(template.DNSNames, name)
		}
	}
	return template
}

// clusterIssuer is the certificate that names the cluster key as the issuer. The key is not
// in a certificate of its own: clients know it by its public key.
func clusterIssuer(clusterKey cluster.Key) *x509.Certificate {
	return &x509.Certificate{Subject: pkix.Name{CommonName: "splicefs cluster " + clusterKey.ID}}
}

// usesGeneratedCert reports whether the listener serves the certificate that the node makes.
func usesGeneratedCert(entry config.Endpoint) bool {
	return !entry.Plain && entry.TLS == nil
}

// certNamesFor returns the names of the certificate of the node of cfg, sorted, each once: the
// host of the cluster URL, the node name, host, and the addresses that the listeners of the
// generated certificate listen on. A name that cannot be in a certificate is left out, and so
// is an address that matches every interface.
func certNamesFor(cfg *config.Config, host string) []string {
	candidates := []string{cfg.Cluster.Node, host}
	if u, err := url.Parse(cfg.Cluster.URL); err == nil {
		candidates = append(candidates, u.Hostname())
	}
	for _, entry := range cfg.Listen {
		if usesGeneratedCert(entry) && !entry.Address.IsUnspecified() {
			candidates = append(candidates, entry.Address.String())
		}
	}
	return joinNames(candidates)
}

// joinNames returns the usable names of all lists, sorted, each once, an address in its
// standard form.
func joinNames(lists ...[]string) []string {
	var names []string
	for _, list := range lists {
		for _, name := range list {
			if name = certHost(name); name == "" {
				continue
			}
			if ip := net.ParseIP(name); ip != nil {
				name = ip.String()
			}
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return slices.Compact(names)
}
