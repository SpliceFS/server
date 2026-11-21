package auth

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/SpliceFS/common/api"
)

// Leeway is the clock difference tolerated between nodes when checking the "exp" and
// "iat" claims of a token.
const Leeway = 30 * time.Second

// Token header values.
const (
	tokenAlgorithm = "EdDSA"
	tokenType      = "JWT"
	tokenSegments  = 3 // header, payload and signature
)

// tokenEncoding is the base64 flavor of JWT segments: URL alphabet, no padding.
func tokenEncoding() *base64.Encoding {
	return base64.RawURLEncoding
}

// tokenHeader is the JWT header. Kid is the id of the node that issued the token.
type tokenHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
	Kid string `json:"kid"`
}

// tokenPayload is the JWT payload. Sub is the id of the user: the name is a label that can
// change, and what the user may do is read from the state of the user, so the token holds
// neither. The times are pointers so that a missing claim can be told from zero.
type tokenPayload struct {
	Sub string `json:"sub"`
	Iat *int64 `json:"iat"`
	Exp *int64 `json:"exp"`
}

// TokenClaims is what a genuine token states. It names the user by id, never by name, so
// that an id is not taken for a name.
type TokenClaims struct {
	UserID  string
	Expires time.Time
}

// Tokens issues and verifies tokens: compact JWTs signed with the Ed25519 key of the node
// that issues them. A node checks a token of another node against the signing key in the
// file of that node, which the node list serves, so a token is accepted as soon as the sync
// tool has delivered that file. A node accepts its own tokens without the list. It is safe
// for concurrent use.
type Tokens struct {
	nodeID  string
	signKey ed25519.PrivateKey
	nodes   api.NodeLister
	ttl     time.Duration
	now     func() time.Time
	marshal func(any) ([]byte, error)
}

// NewTokens returns the Tokens of the node nodeID, which signs with signKey and issues tokens
// valid for ttl. The tokens of every other node are checked against nodes.
func NewTokens(
	nodeID string, signKey ed25519.PrivateKey, nodes api.NodeLister, ttl time.Duration,
	now func() time.Time,
) *Tokens {
	return &Tokens{
		nodeID: nodeID, signKey: signKey, nodes: nodes, ttl: ttl, now: now, marshal: json.Marshal,
	}
}

// Issue returns a token for the user with the given id, and its expiry (whole seconds).
func (t *Tokens) Issue(userID string) (string, time.Time, error) {
	now := t.now()
	issuedSec, expiresSec := now.Unix(), now.Add(t.ttl).Unix()
	header, err := t.segment(tokenHeader{Alg: tokenAlgorithm, Typ: tokenType, Kid: t.nodeID})
	if err != nil {
		return "", time.Time{}, err
	}
	payload, err := t.segment(tokenPayload{Sub: userID, Iat: &issuedSec, Exp: &expiresSec})
	if err != nil {
		return "", time.Time{}, err
	}
	input := header + "." + payload
	signature := ed25519.Sign(t.signKey, []byte(input))
	return input + "." + tokenEncoding().EncodeToString(signature), time.Unix(expiresSec, 0), nil
}

// Verify checks the token and returns what it states. It returns api.ErrInvalidToken for a
// token that is malformed, has an algorithm other than EdDSA, names a node whose signing key
// is not known or has a bad signature, or was issued more than Leeway in the future, and
// api.ErrExpiredToken for one that expired more than Leeway ago. The signature is checked
// before any claim is used. A failure to read the node list is returned as it is.
func (t *Tokens) Verify(token string) (TokenClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != tokenSegments {
		return TokenClaims{}, invalidToken("wrong number of segments")
	}
	var header tokenHeader
	if err := decodeSegment(parts[0], &header); err != nil {
		return TokenClaims{}, invalidToken("bad header")
	}
	if header.Alg != tokenAlgorithm {
		return TokenClaims{}, invalidToken("unsupported algorithm")
	}
	if err := t.checkSignature(header.Kid, parts); err != nil {
		return TokenClaims{}, err
	}
	var payload tokenPayload
	if err := decodeSegment(parts[1], &payload); err != nil {
		return TokenClaims{}, invalidToken("bad payload")
	}
	return t.claims(payload)
}

// checkSignature checks that the third of the segments signs the first two with the key of
// the node kid.
func (t *Tokens) checkSignature(kid string, parts []string) error {
	key, err := t.verifyKey(kid)
	if err != nil {
		return err
	}
	signature, err := tokenEncoding().DecodeString(parts[2])
	if err != nil {
		return invalidToken("bad signature encoding")
	}
	if !ed25519.Verify(key, []byte(parts[0]+"."+parts[1]), signature) {
		return invalidToken("bad signature")
	}
	return nil
}

// verifyKey returns the public key that checks the tokens of the node kid: this node's own, or
// the signing key in the node list. A node without a usable key, such as one that runs a
// version that does not publish it, has no tokens that can be checked.
func (t *Tokens) verifyKey(kid string) (ed25519.PublicKey, error) {
	if kid == t.nodeID {
		// An Ed25519 private key is its seed followed by its public key.
		return ed25519.PublicKey(t.signKey[ed25519.SeedSize:]), nil
	}
	list, err := t.nodes.Nodes()
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	for _, node := range list.Nodes {
		if node.ID == kid {
			return publicKey(node.SignKey)
		}
	}
	return nil, invalidToken("unknown key id")
}

// publicKey returns the Ed25519 key of its base64 text. The node loader checks the text, but
// a key of the wrong size would make Verify panic, so it is checked here as well.
func publicKey(text string) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(text)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, invalidToken("the node has no usable signing key")
	}
	return raw, nil
}

// claims turns an authenticated payload into TokenClaims, checking the claims and the time.
func (t *Tokens) claims(payload tokenPayload) (TokenClaims, error) {
	if payload.Sub == "" || payload.Iat == nil || payload.Exp == nil {
		return TokenClaims{}, invalidToken("missing claim")
	}
	now := t.now()
	if time.Unix(*payload.Iat, 0).Sub(now) > Leeway {
		return TokenClaims{}, invalidToken("issued in the future")
	}
	expires := time.Unix(*payload.Exp, 0)
	if now.Sub(expires) > Leeway {
		return TokenClaims{}, api.ErrExpiredToken
	}
	return TokenClaims{UserID: payload.Sub, Expires: expires}, nil
}

// segment returns v as JSON in base64url.
func (t *Tokens) segment(v any) (string, error) {
	data, err := t.marshal(v)
	if err != nil {
		return "", fmt.Errorf("encode token: %w", err)
	}
	return tokenEncoding().EncodeToString(data), nil
}

// invalidToken returns api.ErrInvalidToken with a reason that never contains token content.
func invalidToken(reason string) error {
	return fmt.Errorf("%w: %s", api.ErrInvalidToken, reason)
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
