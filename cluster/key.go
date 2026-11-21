package cluster

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/SpliceFS/server/internal/uuid7"
)

// KeyFile is the name of the cluster key file in the cluster folder of the shared state.
const KeyFile = "key.yaml"

// The errors of a bad key file. They only reach the log, and the file holds a secret, so none
// of them has any text of the file.
var (
	errKeyFileForm = errors.New("must be YAML with the keys id, private_key and public_key, each set once")
	errKeyID       = errors.New("id must be a UUIDv7 in lowercase")
	errKeyMismatch = errors.New("public_key must be the public key of private_key")
	errNotKeyFile  = errors.New("only " + KeyFile + " is the cluster key file")
)

// Key is the key pair of the cluster: one Ed25519 key that every node holds, so that what it
// signs is trusted as the cluster's.
type Key struct {
	// ID names the key. It is a UUIDv7.
	ID string
	// Private is the Ed25519 private key: the seed, then the public key.
	Private ed25519.PrivateKey
}

// keyFile is the YAML layout of the key file. The private key is the base64 of the 32-byte
// seed, and the public key is the base64 of the 32-byte public key, so a reader can compare
// the public key as text.
type keyFile struct {
	ID         string `yaml:"id"`
	PrivateKey string `yaml:"private_key"`
	PublicKey  string `yaml:"public_key"`
}

// NewKey makes a key for the time now. The seed of the key and then the bits of the id come
// from random.
func NewKey(now time.Time, random io.Reader) (Key, error) {
	seed := make([]byte, ed25519.SeedSize)
	if _, err := io.ReadFull(random, seed); err != nil {
		return Key{}, fmt.Errorf("read random bytes: %w", err)
	}
	id, err := uuid7.New(now, random)
	if err != nil {
		return Key{}, fmt.Errorf("make the key id: %w", err)
	}
	return Key{ID: id, Private: ed25519.NewKeyFromSeed(seed)}, nil
}

// Equal reports whether other is the same key: the same id and the same private key.
func (k Key) Equal(other Key) bool {
	return k.ID == other.ID && k.Private.Equal(other.Private)
}

// PublicText returns the public key as text: the base64 that the file and a client's
// configuration hold.
func (k Key) PublicText() string {
	return base64.StdEncoding.EncodeToString(k.Private[ed25519.SeedSize:])
}

// MarshalKey returns the content of the key file of k, the layout that ParseKey reads.
func MarshalKey(k Key) []byte {
	seed := base64.StdEncoding.EncodeToString(k.Private.Seed())
	// A struct of three strings always encodes, so there is no error to handle.
	//nolint:errcheck,gosec // cannot fail for strings; holding the private key is what the file is for
	data, _ := yaml.Marshal(keyFile{ID: k.ID, PrivateKey: seed, PublicKey: k.PublicText()})
	return data
}

// ParseKey returns the key that the content of a key file holds. The public key in the file
// must be the one of the private key: a file where they differ was edited by hand or damaged,
// and the key that clients are told would not be the key that signs.
func ParseKey(data []byte) (Key, error) {
	var file keyFile
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	// The text of the decoder is not passed on: it may quote a value of the file.
	if err := dec.Decode(&file); err != nil {
		return Key{}, errKeyFileForm
	}
	if !uuid7.Valid(file.ID) {
		return Key{}, errKeyID
	}
	seed, err := decodeKey(file.PrivateKey)
	if err != nil {
		return Key{}, fmt.Errorf("private_key %w", err)
	}
	if _, err = decodeKey(file.PublicKey); err != nil {
		return Key{}, fmt.Errorf("public_key %w", err)
	}
	parsed := Key{ID: file.ID, Private: ed25519.NewKeyFromSeed(seed)}
	if parsed.PublicText() != file.PublicKey {
		return Key{}, errKeyMismatch
	}
	return parsed, nil
}
