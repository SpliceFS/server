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

// Hash parsing errors. The text of a reason never contains the hash.
var (
	errEmptyPassword = errors.New("empty password")
	errInvalidHash   = errors.New("invalid password hash")
)

// costFieldCount is the number of values in the "m=<KiB>,t=<passes>,p=<threads>" field.
const costFieldCount = 3

// phcEncoding is the base64 flavor of the PHC string format: standard alphabet, no padding.
func phcEncoding() *base64.Encoding {
	return base64.RawStdEncoding
}

// invalidHash returns errInvalidHash with a reason that never contains hash content.
func invalidHash(reason string) error {
	return fmt.Errorf("%w: %s", errInvalidHash, reason)
}

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
		return "", fmt.Errorf("hash password: %w", errEmptyPassword)
	}
	salt := make([]byte, saltLength)
	if _, err := io.ReadFull(random, salt); err != nil {
		return "", fmt.Errorf("hash password: read salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, hashTime, hashMemory, hashThreads, keyLength)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, hashMemory, hashTime, hashThreads,
		phcEncoding().EncodeToString(salt), phcEncoding().EncodeToString(key)), nil
}

// VerifyPassword reports whether password matches the argon2id PHC string encoded. The
// parameters in the string are used for the computation. A malformed or unsupported
// string is an error; a well-formed string that does not match returns false and nil.
func VerifyPassword(encoded, password string) (bool, error) {
	params, err := parseHash(encoded)
	if err != nil {
		return false, err
	}
	threads := uint8(params.threads)  //nolint:gosec // parseCosts bounds it to maxThreads
	keyLen := uint32(len(params.key)) //nolint:gosec // a decoded field of one config line cannot exceed uint32
	key := argon2.IDKey([]byte(password), params.salt, params.time, params.memory, threads, keyLen)
	return subtle.ConstantTimeCompare(key, params.key) == 1, nil
}

// parseHash splits and validates an argon2id PHC string. Errors never contain the string.
func parseHash(encoded string) (hashParams, error) {
	fields := strings.Split(encoded, "$")
	if len(fields) != 6 || fields[0] != "" {
		return hashParams{}, invalidHash("wrong number of fields")
	}
	if fields[1] != "argon2id" {
		return hashParams{}, invalidHash("unsupported algorithm")
	}
	if fields[2] != "v="+strconv.Itoa(argon2.Version) {
		return hashParams{}, invalidHash("unsupported version")
	}
	params, err := parseCosts(fields[3])
	if err != nil {
		return hashParams{}, err
	}
	if params.salt, err = phcEncoding().DecodeString(fields[4]); err != nil || len(params.salt) == 0 {
		return hashParams{}, invalidHash("bad salt")
	}
	if params.key, err = phcEncoding().DecodeString(fields[5]); err != nil || len(params.key) == 0 {
		return hashParams{}, invalidHash("bad hash")
	}
	return params, nil
}

// parseCosts parses and bounds the "m=<KiB>,t=<passes>,p=<threads>" field.
func parseCosts(field string) (hashParams, error) {
	parts := strings.Split(field, ",")
	if len(parts) != costFieldCount {
		return hashParams{}, invalidHash("bad parameters")
	}
	var values [costFieldCount]uint32
	for i, prefix := range [costFieldCount]string{"m=", "t=", "p="} {
		value, ok := parseCostValue(parts[i], prefix)
		if !ok {
			return hashParams{}, invalidHash("bad parameters")
		}
		values[i] = value
	}
	params := hashParams{memory: values[0], time: values[1], threads: values[2]}
	if !costsInRange(params) {
		return hashParams{}, invalidHash("parameters out of range")
	}
	return params, nil
}

// parseCostValue reads one "<prefix><number>" part of the costs field.
func parseCostValue(part, prefix string) (uint32, bool) {
	rest, ok := strings.CutPrefix(part, prefix)
	if !ok {
		return 0, false
	}
	value, err := strconv.ParseUint(rest, 10, 32)
	if err != nil {
		return 0, false
	}
	return uint32(value), true
}

// costsInRange reports whether every cost of params is at least 1 and within its upper bound.
func costsInRange(params hashParams) bool {
	return params.memory >= 1 && params.memory <= maxMemory &&
		params.time >= 1 && params.time <= maxTime &&
		params.threads >= 1 && params.threads <= maxThreads
}
