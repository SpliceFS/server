package app

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"io"

	"go.yaml.in/yaml/v3"
)

// nodeKeysMode is the mode of the node keys file. Only this machine's server reads it.
const nodeKeysMode = 0o600

// keySize is the size in bytes of every key in the node keys file: an Ed25519 seed and an
// X25519 private key.
const keySize = 32

// ErrMalformedNodeKeys is the cause of a start that fails because the saved node keys are not
// two keys. The text goes after the name of the file.
var ErrMalformedNodeKeys = errors.New("must hold sign and encrypt, each the base64 of 32 bytes")

// errKeySize is the cause of a key of the wrong length.
var errKeySize = errors.New("must be 32 bytes")

// nodeKeys are the keys of this node: one signs, one receives what other nodes encrypt.
type nodeKeys struct {
	sign    ed25519.PrivateKey
	encrypt *ecdh.PrivateKey
}

// keysFile is the form of the node keys file: the private halves, base64.
type keysFile struct {
	Sign    string `yaml:"sign"`
	Encrypt string `yaml:"encrypt"`
}

// nodeKeys returns the keys of this node, making and saving them on the first start, in the
// same step as the node id. Keys that are saved but malformed are an error and are never
// replaced: other nodes know the public halves, and new keys would be a different node.
func (d *dataPaths) nodeKeys(random io.Reader) (nodeKeys, error) {
	return localValue[nodeKeys]{
		what:   "node keys",
		path:   d.nodeKeysFile,
		mode:   nodeKeysMode,
		parse:  parseNodeKeys,
		create: func() ([]byte, error) { return newNodeKeysFile(random) },
	}.load()
}

// publicKeys are the public halves of the node keys, base64, as the node announces them.
type publicKeys struct {
	sign, encrypt string
}

// public returns the public halves of the keys.
func (k nodeKeys) public() publicKeys {
	enc := base64.StdEncoding
	return publicKeys{
		// An Ed25519 private key is its seed followed by its public key.
		sign:    enc.EncodeToString(k.sign[ed25519.SeedSize:]),
		encrypt: enc.EncodeToString(k.encrypt.PublicKey().Bytes()),
	}
}

// newNodeKeysFile returns the content of a node keys file with new keys.
func newNodeKeysFile(random io.Reader) ([]byte, error) {
	var secrets [2 * keySize]byte
	if _, err := io.ReadFull(random, secrets[:]); err != nil {
		return nil, fmt.Errorf("read random bytes: %w", err)
	}
	enc := base64.StdEncoding
	file := keysFile{Sign: enc.EncodeToString(secrets[:keySize]), Encrypt: enc.EncodeToString(secrets[keySize:])}
	data, _ := yaml.Marshal(file) //nolint:errcheck // cannot fail for a struct of two strings
	return data, nil
}

// parseNodeKeys returns the keys that the content of a node keys file holds.
func parseNodeKeys(data []byte) (nodeKeys, error) {
	var file keysFile
	if err := yaml.Unmarshal(data, &file); err != nil {
		return nodeKeys{}, fmt.Errorf("%w: %w", ErrMalformedNodeKeys, err)
	}
	sign, err := parseSignKey(file.Sign)
	if err != nil {
		return nodeKeys{}, fmt.Errorf("%w: sign: %w", ErrMalformedNodeKeys, err)
	}
	encrypt, err := parseEncryptKey(file.Encrypt)
	if err != nil {
		return nodeKeys{}, fmt.Errorf("%w: encrypt: %w", ErrMalformedNodeKeys, err)
	}
	return nodeKeys{sign: sign, encrypt: encrypt}, nil
}

// parseSignKey returns the Ed25519 key of a base64 seed.
func parseSignKey(text string) (ed25519.PrivateKey, error) {
	seed, err := base64.StdEncoding.DecodeString(text)
	if err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	if len(seed) != ed25519.SeedSize {
		return nil, errKeySize
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// parseEncryptKey returns the X25519 key of a base64 private key; the length is checked there.
func parseEncryptKey(text string) (*ecdh.PrivateKey, error) {
	raw, err := base64.StdEncoding.DecodeString(text)
	if err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	key, err := ecdh.X25519().NewPrivateKey(raw)
	if err != nil {
		return nil, fmt.Errorf("check: %w", err)
	}
	return key, nil
}
