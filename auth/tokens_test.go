package auth

import (
	"bytes"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SpliceFS/common/api"
	"github.com/SpliceFS/common/wire"
	"github.com/SpliceFS/server/cluster"
)

// testNode is the node id of the node under test; other nodes use otherNode.
const (
	testNode  = "00000000-0000-7000-8000-000000000001"
	otherNode = "00000000-0000-7000-8000-000000000002"
)

// keyOf returns the Ed25519 key made from a seed of 32 copies of b, so that every test key is
// the same on every run.
func keyOf(b byte) ed25519.PrivateKey {
	return ed25519.NewKeyFromSeed(bytes.Repeat([]byte{b}, ed25519.SeedSize))
}

// publicText returns the public half of key as a node file spells it: base64, standard alphabet.
func publicText(key ed25519.PrivateKey) string {
	return base64.StdEncoding.EncodeToString(key[ed25519.SeedSize:])
}

// stubNodes is a node list that a test sets.
type stubNodes struct {
	nodes []wire.Node
	err   error
}

func (s stubNodes) Nodes() (wire.Nodes, error) {
	return wire.Nodes{Nodes: s.nodes}, s.err
}

// otherNodeEntry is the entry of otherNode in a node list, with the public key of keyOf(2).
func otherNodeEntry() wire.Node {
	return wire.Node{Name: "other", URL: "https://other.example.com", ID: otherNode, SignKey: publicText(keyOf(2))}
}

// newTestTokens returns the Tokens of testNode, signing with keyOf(1), over the given nodes.
func newTestTokens(nodes api.NodeLister, clock *fakeClock) *Tokens {
	return NewTokens(testNode, keyOf(1), nodes, 30*time.Minute, clock.Now)
}

// forge builds a token from raw segments, signed with key over them.
func forge(key ed25519.PrivateKey, header, payload string) string {
	input := header + "." + payload
	return input + "." + tokenEncoding().EncodeToString(ed25519.Sign(key, []byte(input)))
}

// forgeHMAC builds a token from raw segments with an HMAC-SHA256 signature, as earlier
// versions made them.
func forgeHMAC(secret, header, payload string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	input := header + "." + payload
	// hash.Hash.Write never returns an error, by its documentation.
	_, _ = mac.Write([]byte(input))
	return input + "." + tokenEncoding().EncodeToString(mac.Sum(nil))
}

func jsonSegment(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return tokenEncoding().EncodeToString(data)
}

type obj = map[string]any

// headerOf returns the header segment of a token that names node kid and algorithm alg.
func headerOf(t *testing.T, alg, kid string) string {
	t.Helper()
	return jsonSegment(t, obj{"alg": alg, "typ": "JWT", "kid": kid})
}

func TestTokensIssueAndVerify(t *testing.T) {
	t.Parallel()
	clock := newFakeClock()
	tokens := newTestTokens(stubNodes{}, clock)
	token, expires, err := tokens.Issue(aliceID)
	if err != nil {
		t.Fatal(err)
	}
	wantExpires := clock.Now().Add(30 * time.Minute)
	if !expires.Equal(wantExpires) {
		t.Errorf("expires = %v, want %v", expires, wantExpires)
	}
	claims, err := tokens.Verify(token)
	if err != nil {
		t.Fatal(err)
	}
	if claims.UserID != aliceID || !claims.Expires.Equal(wantExpires) {
		t.Errorf("claims = %+v, want the id of alice and %v", claims, wantExpires)
	}
}

func TestTokensIssueLayout(t *testing.T) {
	t.Parallel()
	clock := newFakeClock()
	tokens := newTestTokens(stubNodes{}, clock)
	token, _, err := tokens.Issue(aliceID)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d segments", len(parts))
	}
	header, err := tokenEncoding().DecodeString(parts[0])
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"alg":"EdDSA","typ":"JWT","kid":"` + testNode + `"}`; string(header) != want {
		t.Errorf("header = %s, want %s", header, want)
	}
	payload, err := tokenEncoding().DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	nowSec := clock.Now().Unix()
	wantPayload := `{"sub":"` + aliceID + `","iat":` + strconv.FormatInt(nowSec, 10) +
		`,"exp":` + strconv.FormatInt(nowSec+1800, 10) + `}`
	if string(payload) != wantPayload {
		t.Errorf("payload = %s, want %s", payload, wantPayload)
	}
	signature, err := tokenEncoding().DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	public := ed25519.PublicKey(keyOf(1)[ed25519.SeedSize:])
	if !ed25519.Verify(public, []byte(parts[0]+"."+parts[1]), signature) {
		t.Error("signature is not Ed25519 over the first two segments with the key of the node")
	}
}

func TestTokensVerifyOwnTokenBeforeTheNodeFileExists(t *testing.T) {
	t.Parallel()
	// The list fails when asked: a token of this node must not need it.
	tokens := newTestTokens(stubNodes{err: errors.New("list failed")}, newFakeClock())
	token, _, err := tokens.Issue(aliceID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tokens.Verify(token); err != nil {
		t.Errorf("own token: %v", err)
	}
}

func TestTokensVerifyTokenOfAnotherNode(t *testing.T) {
	t.Parallel()
	clock := newFakeClock()
	other := NewTokens(otherNode, keyOf(2), stubNodes{}, 30*time.Minute, clock.Now)
	token, _, err := other.Issue(bobID)
	if err != nil {
		t.Fatal(err)
	}
	tokens := newTestTokens(stubNodes{nodes: []wire.Node{otherNodeEntry()}}, clock)
	claims, err := tokens.Verify(token)
	if err != nil {
		t.Fatal(err)
	}
	if claims.UserID != bobID {
		t.Errorf("claims = %+v, want the id of bob", claims)
	}
}

// TestTokensVerifyFollowsTheNodeFile runs the real node loader: a token of another node is
// refused until the sync tool has delivered that node's file, and accepted after.
func TestTokensVerifyFollowsTheNodeFile(t *testing.T) {
	t.Parallel()
	clock := newFakeClock()
	dir := t.TempDir()
	tokens := newTestTokens(cluster.NewLoader(dir, slog.New(slog.DiscardHandler)), clock)
	other := NewTokens(otherNode, keyOf(2), stubNodes{}, 30*time.Minute, clock.Now)
	token, _, err := other.Issue(bobID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tokens.Verify(token); !errors.Is(err, api.ErrInvalidToken) {
		t.Fatalf("before the node file: err = %v, want api.ErrInvalidToken", err)
	}
	entry := otherNodeEntry()
	entry.EncryptKey = publicText(keyOf(3))
	if err := cluster.Announce(dir, otherNode, entry); err != nil {
		t.Fatal(err)
	}
	if _, err := tokens.Verify(token); err != nil {
		t.Errorf("after the node file: %v", err)
	}
}

func TestTokensVerifyListFailure(t *testing.T) {
	t.Parallel()
	cause := errors.New("list failed")
	tokens := newTestTokens(stubNodes{err: cause}, newFakeClock())
	header := headerOf(t, "EdDSA", otherNode)
	_, err := tokens.Verify(forge(keyOf(2), header, "e30"))
	if !errors.Is(err, cause) || errors.Is(err, api.ErrInvalidToken) {
		t.Errorf("err = %v, want the cause of the list, not api.ErrInvalidToken", err)
	}
}

func TestTokensVerifyInvalid(t *testing.T) {
	t.Parallel()
	clock := newFakeClock()
	nodes := stubNodes{nodes: []wire.Node{
		otherNodeEntry(),
		{Name: "old", URL: "https://old.example.com"},
		{Name: "bad", URL: "https://bad.example.com", ID: "00000000-0000-7000-8000-000000000003", SignKey: "!!!"},
		{
			Name: "short", URL: "https://short.example.com", ID: "00000000-0000-7000-8000-000000000004",
			SignKey: base64.StdEncoding.EncodeToString([]byte("short")),
		},
		{Name: "gap", URL: "https://gap.example.com", ID: "00000000-0000-7000-8000-000000000005"},
	}}
	tokens := newTestTokens(nodes, clock)
	nowSec := clock.Now().Unix()
	goodHeader := headerOf(t, "EdDSA", testNode)
	goodPayload := jsonSegment(t, obj{"sub": aliceID, "iat": nowSec, "exp": nowSec + 60})
	notJSON := tokenEncoding().EncodeToString([]byte("not json"))
	valid := forge(keyOf(1), goodHeader, goodPayload)
	validParts := strings.Split(valid, ".")
	swap := "A"
	if validParts[2][:1] == "A" {
		swap = "B"
	}
	flipped := validParts[0] + "." + validParts[1] + "." + swap + validParts[2][1:]
	alteredPayload := jsonSegment(t, obj{"sub": bobID, "iat": nowSec, "exp": nowSec + 60})
	tests := []struct {
		name  string
		token string
	}{
		{"empty", ""},
		{"one segment", "abc"},
		{"two segments", validParts[0] + "." + validParts[1]},
		{"four segments", valid + ".x"},
		{"header not base64", "!!!." + goodPayload + "." + validParts[2]},
		{"header not JSON", forge(keyOf(1), notJSON, goodPayload)},
		{"header wrong type", forge(keyOf(1), jsonSegment(t, obj{"alg": 5}), goodPayload)},
		{"alg HS256 signed with a key of the cluster", forgeHMAC(publicText(keyOf(1)),
			headerOf(t, "HS256", testNode), goodPayload)},
		{"alg HS256 signed with a made up secret", forgeHMAC("secret", headerOf(t, "HS256", testNode), goodPayload)},
		{"alg none", forge(keyOf(1), headerOf(t, "none", testNode), goodPayload)},
		{"alg none unsigned", headerOf(t, "none", testNode) + "." + goodPayload + "."},
		{"alg lowercase", forge(keyOf(1), headerOf(t, "eddsa", testNode), goodPayload)},
		{"alg Ed25519", forge(keyOf(1), headerOf(t, "Ed25519", testNode), goodPayload)},
		{"alg missing", forge(keyOf(1), jsonSegment(t, obj{"kid": testNode}), goodPayload)},
		{"unknown kid", forge(keyOf(1), headerOf(t, "EdDSA", "00000000-0000-7000-8000-0000000000ff"), goodPayload)},
		{"kid not a node id", forge(keyOf(1), headerOf(t, "EdDSA", "new"), goodPayload)},
		{"missing kid", forge(keyOf(1), jsonSegment(t, obj{"alg": "EdDSA"}), goodPayload)},
		{"empty kid, which only a node entry without an id has", forge(keyOf(2), headerOf(t, "EdDSA", ""), goodPayload)},
		{"node whose key is not base64", forge(keyOf(2), headerOf(t, "EdDSA", "00000000-0000-7000-8000-000000000003"),
			goodPayload)},
		{"node whose key is short", forge(keyOf(2), headerOf(t, "EdDSA", "00000000-0000-7000-8000-000000000004"),
			goodPayload)},
		{"node entry with an id and no key", forge(keyOf(2), headerOf(t, "EdDSA", "00000000-0000-7000-8000-000000000005"),
			goodPayload)},
		{"signature not base64", validParts[0] + "." + validParts[1] + ".!!!"},
		{"empty signature", validParts[0] + "." + validParts[1] + "."},
		{"short signature", validParts[0] + "." + validParts[1] + "." + validParts[2][:10]},
		{"altered signature", flipped},
		{"signed by the key of another node", forge(keyOf(2), goodHeader, goodPayload)},
		{"another node's name on a token of this node's key", forge(keyOf(1), headerOf(t, "EdDSA", otherNode), goodPayload)},
		{"payload altered after signing", validParts[0] + "." + alteredPayload + "." + validParts[2]},
		{"payload not base64", forge(keyOf(1), goodHeader, "!!!")},
		{"payload not JSON", forge(keyOf(1), goodHeader, notJSON)},
		{"payload wrong type", forge(keyOf(1), goodHeader, jsonSegment(t, obj{"sub": 7}))},
		{"no sub", forge(keyOf(1), goodHeader, jsonSegment(t, obj{"shares": []string{}, "iat": nowSec, "exp": nowSec + 60}))},
		{"empty sub", forge(keyOf(1), goodHeader, jsonSegment(t, obj{"sub": "", "iat": nowSec, "exp": nowSec + 60}))},
		{"no exp", forge(keyOf(1), goodHeader, jsonSegment(t, obj{"sub": "a", "iat": nowSec}))},
		{"no iat", forge(keyOf(1), goodHeader, jsonSegment(t, obj{"sub": "a", "exp": nowSec + 60}))},
		{"issued in the future", forge(keyOf(1), goodHeader,
			jsonSegment(t, obj{"sub": "a", "iat": nowSec + 31, "exp": nowSec + 600}))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			claims, err := tokens.Verify(tt.token)
			if !errors.Is(err, api.ErrInvalidToken) {
				t.Fatalf("err = %v, want api.ErrInvalidToken", err)
			}
			if claims != (TokenClaims{}) {
				t.Errorf("claims = %+v, want zero", claims)
			}
			if tt.token != "" && strings.Contains(err.Error(), tt.token) {
				t.Errorf("error contains the token: %v", err)
			}
		})
	}
}

func TestTokensVerifyTimeBounds(t *testing.T) {
	t.Parallel()
	clock := newFakeClock()
	tokens := newTestTokens(stubNodes{}, clock)
	nowSec := clock.Now().Unix()
	header := headerOf(t, "EdDSA", testNode)
	mint := func(iat, exp int64) string {
		return forge(keyOf(1), header, jsonSegment(t, obj{"sub": "a", "shares": []string{}, "iat": iat, "exp": exp}))
	}
	leeway := int64(Leeway / time.Second)
	tests := []struct {
		name  string
		token string
		want  error
	}{
		{"expires now", mint(nowSec-60, nowSec), nil},
		{"expired within leeway", mint(nowSec-600, nowSec-leeway), nil},
		{"expired beyond leeway", mint(nowSec-600, nowSec-leeway-1), api.ErrExpiredToken},
		{"expired long ago", mint(nowSec-86400, nowSec-3600), api.ErrExpiredToken},
		{"issued within leeway", mint(nowSec+leeway, nowSec+600), nil},
		{"issued beyond leeway", mint(nowSec+leeway+1, nowSec+600), api.ErrInvalidToken},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := tokens.Verify(tt.token)
			if !errors.Is(err, tt.want) || (tt.want == nil && err != nil) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestTokensVerifyExpiryFollowsClock(t *testing.T) {
	t.Parallel()
	clock := newFakeClock()
	tokens := newTestTokens(stubNodes{}, clock)
	token, _, err := tokens.Issue(aliceID)
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(30*time.Minute + Leeway)
	if _, err := tokens.Verify(token); err != nil {
		t.Errorf("at expiry plus leeway: %v", err)
	}
	clock.Advance(time.Second)
	if _, err := tokens.Verify(token); !errors.Is(err, api.ErrExpiredToken) {
		t.Errorf("past leeway: err = %v, want api.ErrExpiredToken", err)
	}
}

func TestTokensIssueEncodeFailure(t *testing.T) {
	t.Parallel()
	for failAt := range 2 {
		tokens := newTestTokens(stubNodes{}, newFakeClock())
		calls := 0
		tokens.marshal = func(value any) ([]byte, error) {
			call := calls
			calls++
			if call == failAt {
				return nil, errors.New("marshal failed")
			}
			return json.Marshal(value)
		}
		if _, _, err := tokens.Issue(aliceID); err == nil {
			t.Errorf("failAt %d: want error", failAt)
		}
	}
}

func TestTokensConcurrent(t *testing.T) {
	t.Parallel()
	tokens := newTestTokens(stubNodes{}, newFakeClock())
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			token, _, err := tokens.Issue(aliceID)
			if err != nil {
				t.Error(err)
				return
			}
			if _, err := tokens.Verify(token); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
}
