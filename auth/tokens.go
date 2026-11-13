package auth

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/SpliceFS/server/internal/datadir"
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

// Errors of a keys file. The text of one never contains the content of the file, because
// they are logged and a mistake in the file may put a secret where a name belongs.
var (
	errNotNodeFile    = errors.New("the file name is not a node id and .yaml")
	errKeysUnparsable = errors.New("cannot be parsed as a keys file with only a keys list")
	errNoKeys         = errors.New("no keys")
	errKeyHasNoID     = errors.New("has no id")
	errKeyRepeatsID   = errors.New("repeats the id of an earlier key")
	errSecretNotB64   = errors.New("secret is not valid base64")
	errSecretTooShort = errors.New("secret is shorter than " + strconv.Itoa(minSecretSize) + " bytes")
	errNoSigningKey   = errors.New("no signing key for this node")
)

// Token header values and limits.
const (
	tokenAlgorithm = "HS256"
	tokenType      = "JWT"
	tokenSegments  = 3 // header, payload and signature
	minSecretSize  = 32
	ownKeyMode     = 0o600
)

// tokenEncoding is the base64 flavor of JWT segments: URL alphabet, no padding.
func tokenEncoding() *base64.Encoding {
	return base64.RawURLEncoding
}

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

// signingKey is one entry of a keys file.
type signingKey struct {
	id     string
	secret []byte
}

// keysFile is the YAML layout of a keys file.
type keysFile struct {
	Keys []keyEntry `yaml:"keys"`
}

// keyEntry is one key of a keys file, as written.
type keyEntry struct {
	ID     string `yaml:"id"`
	Secret string `yaml:"secret"`
}

// Tokens issues and verifies tokens: compact JWTs signed with HMAC-SHA256. Every node writes
// one keys file, named after its node id, into the shared keys directory. The first key of
// this node's own file signs; every key of every file verifies, so a token issued by one
// node is accepted by the others as soon as the sync tool has delivered its file. The
// directory is followed as it changes (see datadir.Dir). It is safe for concurrent use.
type Tokens struct {
	dir     string
	nodeID  string
	ttl     time.Duration
	now     func() time.Time
	marshal func(any) ([]byte, error)
	random  io.Reader
	log     *slog.Logger

	mu    sync.Mutex
	files *datadir.Dir[[]signingKey]
	ring  *keyring
}

// keyring is every key of the directory, merged.
type keyring struct {
	byID map[string]signingKey
	// signer is the first key of this node's own file, nil while there is none.
	signer *signingKey
}

// NewTokens returns a Tokens that uses the keys directory dir for the node nodeID and issues
// tokens valid for ttl. Problems with a file of the directory are logged to log.
func NewTokens(
	dir, nodeID string, ttl time.Duration, now func() time.Time, log *slog.Logger,
) *Tokens {
	return &Tokens{
		dir: dir, nodeID: nodeID, ttl: ttl, now: now, marshal: json.Marshal,
		random: rand.Reader, log: log,
		files: datadir.NewDir(dir, parseKeysFile, log),
	}
}

// EnsureOwnKey makes the keys file of this node if there is none: one key with a random
// secret. A file that exists is never replaced, because other nodes may hold tokens signed
// by its keys; it must be readable and valid or EnsureOwnKey fails.
func (t *Tokens) EnsureOwnKey() error {
	path := filepath.Join(t.dir, datadir.NodeFile(t.nodeID))
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		if err := t.createOwnKey(path); err != nil {
			return fmt.Errorf("create signing key file %s: %w", path, err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read signing key file: %w", err)
	}
	if _, err := parseKeys(data); err != nil {
		return fmt.Errorf("signing key file %s: %w", path, err)
	}
	return nil
}

// Issue returns a token for user that allows shares, and its expiry (whole seconds).
func (t *Tokens) Issue(user string, shares []string) (string, time.Time, error) {
	key := t.currentRing().signer
	if key == nil {
		return "", time.Time{}, errNoSigningKey
	}
	now := t.now()
	issuedSec, expiresSec := now.Unix(), now.Add(t.ttl).Unix()
	if shares == nil {
		shares = []string{}
	}
	header, err := t.segment(tokenHeader{Alg: tokenAlgorithm, Typ: tokenType, Kid: key.id})
	if err != nil {
		return "", time.Time{}, err
	}
	payload, err := t.segment(tokenPayload{Sub: user, Shares: shares, Iat: &issuedSec, Exp: &expiresSec})
	if err != nil {
		return "", time.Time{}, err
	}
	input := header + "." + payload
	return input + "." + tokenEncoding().EncodeToString(sign(key.secret, input)), time.Unix(expiresSec, 0), nil
}

// Verify checks the token and returns what it states. It returns ErrInvalidToken for a
// token that is malformed, has an algorithm other than HS256, an unknown key id or a bad
// signature, or was issued more than Leeway in the future, and ErrExpiredToken for one
// that expired more than Leeway ago. The signature is checked before any claim is used.
func (t *Tokens) Verify(token string) (Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != tokenSegments {
		return Claims{}, invalidToken("wrong number of segments")
	}
	var header tokenHeader
	if err := decodeSegment(parts[0], &header); err != nil {
		return Claims{}, invalidToken("bad header")
	}
	if header.Alg != tokenAlgorithm {
		return Claims{}, invalidToken("unsupported algorithm")
	}
	key, ok := t.currentRing().byID[header.Kid]
	if !ok {
		return Claims{}, invalidToken("unknown key id")
	}
	signature, err := tokenEncoding().DecodeString(parts[2])
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

// createOwnKey writes the first key file of this node.
func (t *Tokens) createOwnKey(path string) error {
	secret := make([]byte, minSecretSize)
	if _, err := io.ReadFull(t.random, secret); err != nil {
		return fmt.Errorf("read random secret: %w", err)
	}
	content := fmt.Sprintf("keys:\n  - id: %q\n    secret: %q\n",
		t.nodeID+"-1", base64.StdEncoding.EncodeToString(secret))
	if err := datadir.WriteAtomic(path, []byte(content), ownKeyMode); err != nil {
		return fmt.Errorf("write key file: %w", err)
	}
	return nil
}

// claims turns an authenticated payload into Claims, checking the claims and the time.
func (t *Tokens) claims(payload tokenPayload) (Claims, error) {
	if payload.Sub == "" || payload.Iat == nil || payload.Exp == nil {
		return Claims{}, invalidToken("missing claim")
	}
	now := t.now()
	if time.Unix(*payload.Iat, 0).Sub(now) > Leeway {
		return Claims{}, invalidToken("issued in the future")
	}
	expires := time.Unix(*payload.Exp, 0)
	if now.Sub(expires) > Leeway {
		return Claims{}, ErrExpiredToken
	}
	return Claims{User: payload.Sub, Shares: payload.Shares, Expires: expires}, nil
}

// segment returns v as JSON in base64url.
func (t *Tokens) segment(v any) (string, error) {
	data, err := t.marshal(v)
	if err != nil {
		return "", fmt.Errorf("encode token: %w", err)
	}
	return tokenEncoding().EncodeToString(data), nil
}

// invalidToken returns ErrInvalidToken with a reason that never contains token content.
func invalidToken(reason string) error {
	return fmt.Errorf("%w: %s", ErrInvalidToken, reason)
}

// decodeSegment decodes one base64url JSON segment into v.
func decodeSegment(segment string, v any) error {
	data, err := tokenEncoding().DecodeString(segment)
	if err != nil {
		return fmt.Errorf("decode token segment: %w", err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("parse token segment: %w", err)
	}
	return nil
}

// sign returns the HMAC-SHA256 of input.
func sign(secret []byte, input string) []byte {
	mac := hmac.New(sha256.New, secret)
	// hash.Hash.Write never returns an error, by its documentation.
	_, _ = mac.Write([]byte(input))
	return mac.Sum(nil)
}

// currentRing returns the keys, reading again the files that changed since the last call.
func (t *Tokens) currentRing() *keyring {
	t.mu.Lock()
	defer t.mu.Unlock()
	entries, changed := t.files.Refresh()
	if changed || t.ring == nil {
		t.ring = t.merge(entries)
	}
	return t.ring
}

// merge builds the keyring from the files in name order. A key id must be unique, so that a
// token names one key: the file that sorts first keeps it and the clash is logged.
func (t *Tokens) merge(entries []datadir.Entry[[]signingKey]) *keyring {
	ring := &keyring{byID: map[string]signingKey{}}
	owner := map[string]string{}
	own := datadir.NodeFile(t.nodeID)
	for _, entry := range entries {
		for _, key := range entry.Value {
			if first, dup := owner[key.id]; dup {
				// The id is left out: a mistake in a file may put a secret where the
				// id belongs, and this line is logged.
				t.log.Warn("duplicate key id, keeping the first file's key",
					slog.String("kept", first), slog.String("ignored", entry.Name))
				continue
			}
			owner[key.id] = entry.Name
			ring.byID[key.id] = key
		}
		if entry.Name == own {
			ring.signer = &entry.Value[0]
		}
	}
	return ring
}

// parseKeysFile parses a file of the keys directory. The name of a file must be the id of a
// node, so a stray file never adds keys.
func parseKeysFile(name string, data []byte) ([]signingKey, error) {
	if _, ok := datadir.NodeOfFile(name); !ok {
		return nil, errNotNodeFile
	}
	return parseKeys(data)
}

// parseKeys validates the content of a keys file. Its errors never contain the content,
// because they are logged and a mistake in the file may put a secret where a name belongs. They
// name a key by its position for that reason, never by its id.
func parseKeys(data []byte) ([]signingKey, error) {
	var parsed keysFile
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&parsed); err != nil {
		return nil, errKeysUnparsable
	}
	if len(parsed.Keys) == 0 {
		return nil, errNoKeys
	}
	keys := make([]signingKey, 0, len(parsed.Keys))
	seen := map[string]bool{}
	for i, entry := range parsed.Keys {
		if entry.ID == "" {
			return nil, fmt.Errorf("key %d %w", i+1, errKeyHasNoID)
		}
		if seen[entry.ID] {
			return nil, fmt.Errorf("key %d %w", i+1, errKeyRepeatsID)
		}
		seen[entry.ID] = true
		secret, err := base64.StdEncoding.DecodeString(entry.Secret)
		if err != nil {
			return nil, fmt.Errorf("key %d: %w", i+1, errSecretNotB64)
		}
		if len(secret) < minSecretSize {
			return nil, fmt.Errorf("key %d: %w", i+1, errSecretTooShort)
		}
		keys = append(keys, signingKey{id: entry.ID, secret: secret})
	}
	return keys, nil
}
