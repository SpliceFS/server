package auth

import (
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters for new hashes: the second recommended parameter set of RFC 9106.
const (
	hashTime    = 3
	hashMemory  = 64 * 1024 // KiB, so 64 MiB
	hashThreads = 4
	saltLength  = 16
	keyLength   = 32
)

// Upper bounds accepted when verifying, so that a hostile user file cannot make the
// server allocate or compute without limit.
const (
	maxMemory  = 1024 * 1024 // KiB, so 1 GiB
	maxTime    = 10
	maxThreads = 64
)

// phcEncoding is the base64 flavor of the PHC string format: standard alphabet, no padding.
var phcEncoding = base64.RawStdEncoding

// hashParams is a parsed argon2id PHC string.
type hashParams struct {
	memory  uint32
	time    uint32
	threads uint32
	salt    []byte
	key     []byte
}

// HashPassword returns the argon2id hash of password as a PHC string. The salt is read
// from random. An empty password is an error.
func HashPassword(password string, random io.Reader) (string, error) {
	if password == "" {
		return "", errors.New("hash password: empty password")
	}
	salt := make([]byte, saltLength)
	if _, err := io.ReadFull(random, salt); err != nil {
		return "", fmt.Errorf("hash password: read salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, hashTime, hashMemory, hashThreads, keyLength)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, hashMemory, hashTime, hashThreads,
		phcEncoding.EncodeToString(salt), phcEncoding.EncodeToString(key)), nil
}

// VerifyPassword reports whether password matches the argon2id PHC string encoded. The
// parameters in the string are used for the computation. A malformed or unsupported
// string is an error; a well-formed string that does not match returns false and nil.
func VerifyPassword(encoded, password string) (bool, error) {
	h, err := parseHash(encoded)
	if err != nil {
		return false, err
	}
	threads := uint8(h.threads)  //nolint:gosec // parseCosts bounds it to maxThreads
	keyLen := uint32(len(h.key)) //nolint:gosec // a decoded field of one config line cannot exceed uint32
	key := argon2.IDKey([]byte(password), h.salt, h.time, h.memory, threads, keyLen)
	return subtle.ConstantTimeCompare(key, h.key) == 1, nil
}

// parseHash splits and validates an argon2id PHC string. Errors never contain the string.
func parseHash(encoded string) (hashParams, error) {
	fields := strings.Split(encoded, "$")
	if len(fields) != 6 || fields[0] != "" {
		return hashParams{}, errors.New("invalid password hash: wrong number of fields")
	}
	if fields[1] != "argon2id" {
		return hashParams{}, errors.New("invalid password hash: unsupported algorithm")
	}
	if fields[2] != "v="+strconv.Itoa(argon2.Version) {
		return hashParams{}, errors.New("invalid password hash: unsupported version")
	}
	h, err := parseCosts(fields[3])
	if err != nil {
		return hashParams{}, err
	}
	if h.salt, err = phcEncoding.DecodeString(fields[4]); err != nil || len(h.salt) == 0 {
		return hashParams{}, errors.New("invalid password hash: bad salt")
	}
	if h.key, err = phcEncoding.DecodeString(fields[5]); err != nil || len(h.key) == 0 {
		return hashParams{}, errors.New("invalid password hash: bad hash")
	}
	return h, nil
}

// parseCosts parses and bounds the "m=<KiB>,t=<passes>,p=<threads>" field.
func parseCosts(field string) (hashParams, error) {
	parts := strings.Split(field, ",")
	if len(parts) != 3 {
		return hashParams{}, errors.New("invalid password hash: bad parameters")
	}
	var values [3]uint32
	for i, prefix := range [3]string{"m=", "t=", "p="} {
		rest, ok := strings.CutPrefix(parts[i], prefix)
		if !ok {
			return hashParams{}, errors.New("invalid password hash: bad parameters")
		}
		v, err := strconv.ParseUint(rest, 10, 32)
		if err != nil {
			return hashParams{}, errors.New("invalid password hash: bad parameters")
		}
		values[i] = uint32(v)
	}
	h := hashParams{memory: values[0], time: values[1], threads: values[2]}
	if h.memory < 1 || h.memory > maxMemory ||
		h.time < 1 || h.time > maxTime ||
		h.threads < 1 || h.threads > maxThreads {
		return hashParams{}, errors.New("invalid password hash: parameters out of range")
	}
	return h, nil
}
