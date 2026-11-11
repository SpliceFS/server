package auth

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"go.yaml.in/yaml/v3"
)

// Leeway is the clock difference tolerated between nodes when checking the "exp" and
// "iat" claims of a token.
const Leeway = 30 * time.Second

var (
	// ErrInvalidToken means the token is malformed, uses another algorithm, names an
	// unknown key, has a bad signature or is not valid yet. The error wraps the reason.
	ErrInvalidToken = errors.New("invalid token")
	// ErrExpiredToken means the token is genuine but past its expiry.
	ErrExpiredToken = errors.New("expired token")
)

// Token header values and limits.
const (
	tokenAlgorithm = "HS256"
	tokenType      = "JWT"
	minSecretSize  = 32
)

// tokenEncoding is the base64 flavor of JWT segments: URL alphabet, no padding.
var tokenEncoding = base64.RawURLEncoding

// Claims is what a valid token states.
type Claims struct {
	// User is the user name the token was issued to.
	User string
	// Shares lists the shares the token allows; "*" means every share.
	Shares []string
	// Expires is the expiry time carried by the token.
	Expires time.Time
}

// Allows reports whether the token allows the named share.
func (c Claims) Allows(share string) bool {
	return allowsShare(c.Shares, share)
}

// tokenHeader is the JWT header.
type tokenHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
	Kid string `json:"kid"`
}

// tokenPayload is the JWT payload. The times are pointers so that a missing claim can be
// told from zero.
type tokenPayload struct {
	Sub    string   `json:"sub"`
	Shares []string `json:"shares"`
	Iat    *int64   `json:"iat"`
	Exp    *int64   `json:"exp"`
}

// signingKey is one entry of the keys file.
type signingKey struct {
	id     string
	secret []byte
}

// keysFile is the YAML layout of the keys file.
type keysFile struct {
	Keys []struct {
		ID     string `yaml:"id"`
		Secret string `yaml:"secret"`
	} `yaml:"keys"`
}

// Tokens issues and verifies tokens: compact JWTs signed with HMAC-SHA256. The first key
// of the keys file signs and every key verifies. The file is read again when its
// modification time or size changes. It is safe for concurrent use.
type Tokens struct {
	path    string
	ttl     time.Duration
	now     func() time.Time
	marshal func(any) ([]byte, error)

	mu      sync.Mutex
	keys    []signingKey
	modTime time.Time
	size    int64
}

// NewTokens returns a Tokens that reads keysFile and issues tokens valid for ttl.
func NewTokens(keysFile string, ttl time.Duration, now func() time.Time) *Tokens {
	return &Tokens{path: keysFile, ttl: ttl, now: now, marshal: json.Marshal}
}

// Issue returns a token for user that allows shares, and its expiry (whole seconds).
func (t *Tokens) Issue(user string, shares []string) (token string, expires time.Time, err error) {
	keys, err := t.currentKeys()
	if err != nil {
		return "", time.Time{}, err
	}
	key := keys[0]
	now := t.now()
	iat, exp := now.Unix(), now.Add(t.ttl).Unix()
	if shares == nil {
		shares = []string{}
	}
	header, err := t.segment(tokenHeader{Alg: tokenAlgorithm, Typ: tokenType, Kid: key.id})
	if err != nil {
		return "", time.Time{}, err
	}
	payload, err := t.segment(tokenPayload{Sub: user, Shares: shares, Iat: &iat, Exp: &exp})
	if err != nil {
		return "", time.Time{}, err
	}
	input := header + "." + payload
	return input + "." + tokenEncoding.EncodeToString(sign(key.secret, input)), time.Unix(exp, 0), nil
}

// Verify checks the token and returns what it states. It returns ErrInvalidToken for a
// token that is malformed, has an algorithm other than HS256, an unknown key id or a bad
// signature, or was issued more than Leeway in the future, and ErrExpiredToken for one
// that expired more than Leeway ago. The signature is checked before any claim is used.
func (t *Tokens) Verify(token string) (Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Claims{}, invalidToken("wrong number of segments")
	}
	var header tokenHeader
	if err := decodeSegment(parts[0], &header); err != nil {
		return Claims{}, invalidToken("bad header")
	}
	if header.Alg != tokenAlgorithm {
		return Claims{}, invalidToken("unsupported algorithm")
	}
	keys, err := t.currentKeys()
	if err != nil {
		return Claims{}, err
	}
	key, ok := findKey(keys, header.Kid)
	if !ok {
		return Claims{}, invalidToken("unknown key id")
	}
	signature, err := tokenEncoding.DecodeString(parts[2])
	if err != nil {
		return Claims{}, invalidToken("bad signature encoding")
	}
	if !hmac.Equal(signature, sign(key.secret, parts[0]+"."+parts[1])) {
		return Claims{}, invalidToken("bad signature")
	}
	var payload tokenPayload
	if err := decodeSegment(parts[1], &payload); err != nil {
		return Claims{}, invalidToken("bad payload")
	}
	return t.claims(payload)
}

// claims turns an authenticated payload into Claims, checking the claims and the time.
func (t *Tokens) claims(p tokenPayload) (Claims, error) {
	if p.Sub == "" || p.Iat == nil || p.Exp == nil {
		return Claims{}, invalidToken("missing claim")
	}
	now := t.now()
	if time.Unix(*p.Iat, 0).Sub(now) > Leeway {
		return Claims{}, invalidToken("issued in the future")
	}
	expires := time.Unix(*p.Exp, 0)
	if now.Sub(expires) > Leeway {
		return Claims{}, ErrExpiredToken
	}
	return Claims{User: p.Sub, Shares: p.Shares, Expires: expires}, nil
}

// segment returns v as JSON in base64url.
func (t *Tokens) segment(v any) (string, error) {
	data, err := t.marshal(v)
	if err != nil {
		return "", fmt.Errorf("encode token: %w", err)
	}
	return tokenEncoding.EncodeToString(data), nil
}

// invalidToken returns ErrInvalidToken with a reason that never contains token content.
func invalidToken(reason string) error {
	return fmt.Errorf("%w: %s", ErrInvalidToken, reason)
}

// decodeSegment decodes one base64url JSON segment into v.
func decodeSegment(segment string, v any) error {
	data, err := tokenEncoding.DecodeString(segment)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// sign returns the HMAC-SHA256 of input.
func sign(secret []byte, input string) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(input))
	return mac.Sum(nil)
}

// findKey returns the key with the given id.
func findKey(keys []signingKey, id string) (signingKey, bool) {
	for _, k := range keys {
		if k.id == id {
			return k, true
		}
	}
	return signingKey{}, false
}

// currentKeys returns the keys, reading the file again if it changed since the last read.
func (t *Tokens) currentKeys() ([]signingKey, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	info, err := os.Stat(t.path)
	if err != nil {
		return nil, fmt.Errorf("stat keys file: %w", err)
	}
	if t.keys != nil && info.ModTime().Equal(t.modTime) && info.Size() == t.size {
		return t.keys, nil
	}
	keys, err := readKeys(t.path)
	if err != nil {
		return nil, err
	}
	t.keys, t.modTime, t.size = keys, info.ModTime(), info.Size()
	return keys, nil
}

// readKeys reads and validates the keys file.
func readKeys(path string) ([]signingKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read keys file: %w", err)
	}
	var f keysFile
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("parse keys file: %w", err)
	}
	if len(f.Keys) == 0 {
		return nil, errors.New("keys file: no keys")
	}
	keys := make([]signingKey, 0, len(f.Keys))
	for i, entry := range f.Keys {
		if entry.ID == "" {
			return nil, fmt.Errorf("keys file: key %d has no id", i+1)
		}
		if _, dup := findKey(keys, entry.ID); dup {
			return nil, fmt.Errorf("keys file: duplicate key id %q", entry.ID)
		}
		secret, err := base64.StdEncoding.DecodeString(entry.Secret)
		if err != nil {
			return nil, fmt.Errorf("keys file: key %q: secret is not valid base64", entry.ID)
		}
		if len(secret) < minSecretSize {
			return nil, fmt.Errorf("keys file: key %q: secret is shorter than %d bytes", entry.ID, minSecretSize)
		}
		keys = append(keys, signingKey{id: entry.ID, secret: secret})
	}
	return keys, nil
}
