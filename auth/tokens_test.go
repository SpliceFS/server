package auth

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Test secrets, 32 bytes each.
var (
	secretA = []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	secretB = []byte("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
)

func b64(secret []byte) string { return base64.StdEncoding.EncodeToString(secret) }

func keysYAML(entries ...[2]string) string {
	var b strings.Builder
	b.WriteString("keys:\n")
	for _, e := range entries {
		b.WriteString("  - id: \"" + e[0] + "\"\n    secret: \"" + e[1] + "\"\n")
	}
	return b.String()
}

// testNode is the node id of the node under test; other nodes use otherNode.
const (
	testNode  = "0123456789abcdef"
	otherNode = "fedcba9876543210"
)

// discard is a logger for tests that do not look at the log.
var discard = slog.New(slog.DiscardHandler)

// newTestTokens writes the keys file of testNode into a new keys directory and returns
// Tokens that use it, and the path of the file.
func newTestTokens(t *testing.T, content string, clock *fakeClock) (*Tokens, string) {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, dir, testNode+".yaml", content)
	return NewTokens(dir, testNode, 30*time.Minute, clock.Now, discard), filepath.Join(dir, testNode+".yaml")
}

func twoKeys() string {
	return keysYAML([2]string{"new", b64(secretA)}, [2]string{"old", b64(secretB)})
}

// forge builds a token from raw segments, signed with secret over them.
func forge(secret []byte, header, payload string) string {
	input := header + "." + payload
	return input + "." + tokenEncoding.EncodeToString(sign(secret, input))
}

func jsonSegment(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return tokenEncoding.EncodeToString(data)
}

type obj = map[string]any

func TestTokensIssueAndVerify(t *testing.T) {
	t.Parallel()
	clock := newFakeClock()
	tokens, _ := newTestTokens(t, twoKeys(), clock)
	token, expires, err := tokens.Issue("alice", []string{"media", "docs"})
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
	if claims.User != "alice" || !reflect.DeepEqual(claims.Shares, []string{"media", "docs"}) || !claims.Expires.Equal(wantExpires) {
		t.Errorf("claims = %+v, want alice, [media docs], %v", claims, wantExpires)
	}
}

func TestTokensIssueLayout(t *testing.T) {
	t.Parallel()
	clock := newFakeClock()
	tokens, _ := newTestTokens(t, twoKeys(), clock)
	token, _, err := tokens.Issue("alice", nil)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d segments", len(parts))
	}
	header, err := tokenEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(header) != `{"alg":"HS256","typ":"JWT","kid":"new"}` {
		t.Errorf("header = %s", header)
	}
	payload, err := tokenEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	now := clock.Now().Unix()
	wantPayload := `{"sub":"alice","shares":[],"iat":` + strconv.FormatInt(now, 10) + `,"exp":` + strconv.FormatInt(now+1800, 10) + `}`
	if string(payload) != wantPayload {
		t.Errorf("payload = %s, want %s", payload, wantPayload)
	}
	if want := tokenEncoding.EncodeToString(sign(secretA, parts[0]+"."+parts[1])); parts[2] != want {
		t.Error("signature is not HMAC-SHA256 with the first key")
	}
}

func TestTokensVerifyAnyKey(t *testing.T) {
	t.Parallel()
	clock := newFakeClock()
	tokens, _ := newTestTokens(t, twoKeys(), clock)
	now := clock.Now().Unix()
	header := jsonSegment(t, obj{"alg": "HS256", "typ": "JWT", "kid": "old"})
	payload := jsonSegment(t, obj{"sub": "bob", "shares": []string{"*"}, "iat": now, "exp": now + 60})
	claims, err := tokens.Verify(forge(secretB, header, payload))
	if err != nil {
		t.Fatal(err)
	}
	if claims.User != "bob" || !claims.Allows("anything") {
		t.Errorf("claims = %+v", claims)
	}
}

func TestTokensVerifyInvalid(t *testing.T) {
	t.Parallel()
	clock := newFakeClock()
	tokens, _ := newTestTokens(t, twoKeys(), clock)
	now := clock.Now().Unix()
	goodHeader := jsonSegment(t, obj{"alg": "HS256", "typ": "JWT", "kid": "new"})
	goodPayload := jsonSegment(t, obj{"sub": "alice", "shares": []string{"media"}, "iat": now, "exp": now + 60})
	header := func(alg, kid string) string {
		return jsonSegment(t, obj{"alg": alg, "typ": "JWT", "kid": kid})
	}
	notJSON := tokenEncoding.EncodeToString([]byte("not json"))
	valid := forge(secretA, goodHeader, goodPayload)
	validParts := strings.Split(valid, ".")
	swap := "A"
	if validParts[2][:1] == "A" {
		swap = "B"
	}
	flipped := validParts[0] + "." + validParts[1] + "." + swap + validParts[2][1:]
	tests := []struct {
		name  string
		token string
	}{
		{"empty", ""},
		{"one segment", "abc"},
		{"two segments", validParts[0] + "." + validParts[1]},
		{"four segments", valid + ".x"},
		{"header not base64", "!!!." + goodPayload + "." + validParts[2]},
		{"header not JSON", forge(secretA, notJSON, goodPayload)},
		{"header wrong type", forge(secretA, jsonSegment(t, obj{"alg": 5}), goodPayload)},
		{"alg none", forge(secretA, header("none", "new"), goodPayload)},
		{"alg none unsigned", header("none", "new") + "." + goodPayload + "."},
		{"alg lowercase", forge(secretA, header("hs256", "new"), goodPayload)},
		{"alg HS512", forge(secretA, header("HS512", "new"), goodPayload)},
		{"alg missing", forge(secretA, jsonSegment(t, obj{"kid": "new"}), goodPayload)},
		{"unknown kid", forge(secretA, header("HS256", "other"), goodPayload)},
		{"missing kid", forge(secretA, jsonSegment(t, obj{"alg": "HS256"}), goodPayload)},
		{"signature not base64", validParts[0] + "." + validParts[1] + ".!!!"},
		{"empty signature", validParts[0] + "." + validParts[1] + "."},
		{"short signature", validParts[0] + "." + validParts[1] + "." + validParts[2][:10]},
		{"altered signature", flipped},
		{"wrong secret", forge(secretB, goodHeader, goodPayload)},
		{"payload altered after signing", validParts[0] + "." + jsonSegment(t, obj{"sub": "root", "shares": []string{"*"}, "iat": now, "exp": now + 60}) + "." + validParts[2]},
		{"payload not base64", forge(secretA, goodHeader, "!!!")},
		{"payload not JSON", forge(secretA, goodHeader, notJSON)},
		{"payload wrong type", forge(secretA, goodHeader, jsonSegment(t, obj{"sub": 7}))},
		{"no sub", forge(secretA, goodHeader, jsonSegment(t, obj{"shares": []string{}, "iat": now, "exp": now + 60}))},
		{"empty sub", forge(secretA, goodHeader, jsonSegment(t, obj{"sub": "", "iat": now, "exp": now + 60}))},
		{"no exp", forge(secretA, goodHeader, jsonSegment(t, obj{"sub": "a", "iat": now}))},
		{"no iat", forge(secretA, goodHeader, jsonSegment(t, obj{"sub": "a", "exp": now + 60}))},
		{"issued in the future", forge(secretA, goodHeader, jsonSegment(t, obj{"sub": "a", "iat": now + 31, "exp": now + 600}))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			claims, err := tokens.Verify(tt.token)
			if !errors.Is(err, ErrInvalidToken) {
				t.Fatalf("err = %v, want ErrInvalidToken", err)
			}
			if !reflect.DeepEqual(claims, Claims{}) {
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
	tokens, _ := newTestTokens(t, twoKeys(), clock)
	now := clock.Now().Unix()
	header := jsonSegment(t, obj{"alg": "HS256", "typ": "JWT", "kid": "new"})
	mint := func(iat, exp int64) string {
		return forge(secretA, header, jsonSegment(t, obj{"sub": "a", "shares": []string{}, "iat": iat, "exp": exp}))
	}
	leeway := int64(Leeway / time.Second)
	tests := []struct {
		name  string
		token string
		want  error
	}{
		{"expires now", mint(now-60, now), nil},
		{"expired within leeway", mint(now-600, now-leeway), nil},
		{"expired beyond leeway", mint(now-600, now-leeway-1), ErrExpiredToken},
		{"expired long ago", mint(now-86400, now-3600), ErrExpiredToken},
		{"issued within leeway", mint(now+leeway, now+600), nil},
		{"issued beyond leeway", mint(now+leeway+1, now+600), ErrInvalidToken},
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
	tokens, _ := newTestTokens(t, twoKeys(), clock)
	token, _, err := tokens.Issue("alice", []string{"media"})
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(30*time.Minute + Leeway)
	if _, err := tokens.Verify(token); err != nil {
		t.Errorf("at expiry plus leeway: %v", err)
	}
	clock.Advance(time.Second)
	if _, err := tokens.Verify(token); !errors.Is(err, ErrExpiredToken) {
		t.Errorf("past leeway: err = %v, want ErrExpiredToken", err)
	}
}

func TestClaimsAllows(t *testing.T) {
	t.Parallel()
	c := Claims{Shares: []string{"media"}}
	if !c.Allows("media") || c.Allows("docs") {
		t.Error("explicit share list not honored")
	}
	if !(Claims{Shares: []string{"*"}}).Allows("docs") {
		t.Error("wildcard not honored")
	}
}

func TestTokensKeysValidation(t *testing.T) {
	t.Parallel()
	good := b64(secretA)
	tests := []struct {
		name    string
		content string
	}{
		{"empty file", ""},
		{"not YAML", "keys: ["},
		{"no keys key", "other: 1\n"},
		{"empty list", "keys: []\n"},
		{"unknown top-level key", twoKeys() + "extra: 1\n"},
		{"unknown entry key", "keys:\n  - id: a\n    secret: " + good + "\n    note: x\n"},
		{"empty id", keysYAML([2]string{"", good})},
		{"duplicate id", keysYAML([2]string{"a", good}, [2]string{"a", b64(secretB)})},
		{"bad base64", keysYAML([2]string{"a", "!!!not-base64!!!"})},
		{"unpadded base64", keysYAML([2]string{"a", strings.TrimRight(good, "=")})},
		{"short secret", keysYAML([2]string{"a", b64(secretA[:31])})},
		{"empty secret", keysYAML([2]string{"a", ""})},
		{"second key bad", keysYAML([2]string{"a", good}, [2]string{"b", "AAAA"})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tokens, _ := newTestTokens(t, tt.content, newFakeClock())
			if _, _, err := tokens.Issue("alice", nil); err == nil {
				t.Fatal("Issue: want error")
			} else if strings.Contains(err.Error(), good) {
				t.Errorf("error contains the secret: %v", err)
			}
			// An invalid file adds no keys, so nothing it could have signed verifies.
			signed := forge(secretA, jsonSegment(t, obj{"alg": "HS256", "kid": "new"}), jsonSegment(t, obj{}))
			if _, err := tokens.Verify(signed); !errors.Is(err, ErrInvalidToken) {
				t.Errorf("Verify: err = %v, want ErrInvalidToken", err)
			}
		})
	}
}

func TestTokensOwnFileMissing(t *testing.T) {
	t.Parallel()
	tokens := NewTokens(t.TempDir(), testNode, time.Minute, newFakeClock().Now, discard)
	if _, _, err := tokens.Issue("alice", nil); err == nil {
		t.Error("want error")
	}
}

func TestTokensReloadsOnlyWhenFileChanged(t *testing.T) {
	t.Parallel()
	clock := newFakeClock()
	tokens, path := newTestTokens(t, keysYAML([2]string{"one", b64(secretA)}), clock)
	token, _, err := tokens.Issue("alice", nil)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	// Same size and modification time: the cached keys are kept, so the new file is unseen.
	replacement := keysYAML([2]string{"two", b64(secretA)})
	if len(replacement) != int(info.Size()) {
		t.Fatalf("test setup: replacement is %d bytes, want %d", len(replacement), info.Size())
	}
	writeFile(t, filepath.Dir(path), filepath.Base(path), replacement)
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if _, err := tokens.Verify(token); err != nil {
		t.Errorf("cached keys not used: %v", err)
	}

	// Same size, new modification time: reloaded.
	later := info.ModTime().Add(time.Hour)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
	if _, err := tokens.Verify(token); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("after mtime change: err = %v, want ErrInvalidToken (key one removed)", err)
	}
	fresh, _, err := tokens.Issue("alice", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tokens.Verify(fresh); err != nil {
		t.Errorf("token signed with the reloaded key: %v", err)
	}

	// New size, same modification time: reloaded.
	writeFile(t, filepath.Dir(path), filepath.Base(path), keysYAML([2]string{"three", b64(secretB)}, [2]string{"two", b64(secretA)}))
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
	if _, err := tokens.Verify(fresh); err != nil {
		t.Errorf("after size change the old key must still verify: %v", err)
	}
	third, _, err := tokens.Issue("alice", nil)
	if err != nil {
		t.Fatal(err)
	}
	if header := decodeHeader(t, third); header.Kid != "three" {
		t.Errorf("kid = %q, want the new first key", header.Kid)
	}
}

func decodeHeader(t *testing.T, token string) tokenHeader {
	t.Helper()
	var h tokenHeader
	if err := decodeSegment(strings.Split(token, ".")[0], &h); err != nil {
		t.Fatal(err)
	}
	return h
}

func TestTokensBadFileKeepsLastGoodContent(t *testing.T) {
	t.Parallel()
	for name, bad := range map[string]string{
		"syntax error": "keys: [broken\n", "no keys": "keys: []\n", "empty": "",
		"half written": "keys:\n  - id: \"one\"\n    secret: \"" + b64(secretA)[:10],
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			tokens, path := newTestTokens(t, keysYAML([2]string{"one", b64(secretA)}), newFakeClock())
			token, _, err := tokens.Issue("alice", nil)
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Dir(path), filepath.Base(path), bad)
			if _, err := tokens.Verify(token); err != nil {
				t.Errorf("Verify after the file broke: %v", err)
			}
			if _, _, err := tokens.Issue("alice", nil); err != nil {
				t.Errorf("Issue after the file broke: %v", err)
			}
			writeFile(t, filepath.Dir(path), filepath.Base(path), keysYAML([2]string{"two", b64(secretB)}))
			if _, err := tokens.Verify(token); !errors.Is(err, ErrInvalidToken) {
				t.Errorf("after the file is fixed the new content must apply: %v", err)
			}
			if _, _, err := tokens.Issue("alice", nil); err != nil {
				t.Errorf("Issue after the file is fixed: %v", err)
			}
		})
	}
}

func TestTokensIssueEncodeFailure(t *testing.T) {
	t.Parallel()
	for failAt := range 2 {
		tokens, _ := newTestTokens(t, twoKeys(), newFakeClock())
		calls := 0
		tokens.marshal = func(v any) ([]byte, error) {
			if calls++; calls-1 == failAt {
				return nil, errors.New("marshal failed")
			}
			return json.Marshal(v)
		}
		if _, _, err := tokens.Issue("alice", nil); err == nil {
			t.Errorf("failAt %d: want error", failAt)
		}
	}
}

func TestTokensConcurrent(t *testing.T) {
	t.Parallel()
	tokens, _ := newTestTokens(t, twoKeys(), newFakeClock())
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			token, _, err := tokens.Issue("alice", []string{"media"})
			if err != nil {
				t.Error(err)
				return
			}
			if _, err := tokens.Verify(token); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
}

// logged returns a logger that writes to the returned buffer.
func logged() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}

func TestTokensTwoNodesShareKeys(t *testing.T) {
	t.Parallel()
	// Two shared folders stand for two servers; the sync tool is a copy of one file.
	dirA, dirB := t.TempDir(), t.TempDir()
	clock := newFakeClock()
	a := NewTokens(dirA, testNode, time.Minute, clock.Now, discard)
	b := NewTokens(dirB, otherNode, time.Minute, clock.Now, discard)
	for _, tokens := range []*Tokens{a, b} {
		if err := tokens.EnsureOwnKey(); err != nil {
			t.Fatal(err)
		}
	}
	token := mustIssue(t, a)
	if _, err := b.Verify(token); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("before the key file arrives: %v", err)
	}
	copyFile(t, filepath.Join(dirA, testNode+".yaml"), filepath.Join(dirB, testNode+".yaml"))
	if _, err := b.Verify(token); err != nil {
		t.Errorf("after the key file arrives: %v", err)
	}
	if want := []string{testNode + ".yaml", otherNode + ".yaml"}; !reflect.DeepEqual(dirNames(t, dirB), want) {
		t.Errorf("files = %v, want %v", dirNames(t, dirB), want)
	}
	if want := []string{testNode + ".yaml"}; !reflect.DeepEqual(dirNames(t, dirA), want) {
		t.Errorf("node A wrote %v, want only its own file", dirNames(t, dirA))
	}
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	data, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustIssue(t *testing.T, tokens *Tokens) string {
	t.Helper()
	token, _, err := tokens.Issue("bob", nil)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	items, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, item := range items {
		names = append(names, item.Name())
	}
	return names
}

func TestEnsureOwnKeyCreatesTheFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tokens := NewTokens(dir, testNode, time.Minute, newFakeClock().Now, discard)
	if err := tokens.EnsureOwnKey(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, testNode+".yaml")
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v, %v", info, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := parseKeys(data)
	if err != nil || len(keys) != 1 || keys[0].id != testNode+"-1" || len(keys[0].secret) != 32 {
		t.Fatalf("keys = %+v, %v", keys, err)
	}
	if header := decodeHeader(t, mustIssue(t, tokens)); header.Kid != testNode+"-1" {
		t.Errorf("kid = %q", header.Kid)
	}
	// A second start keeps the file, because tokens signed with its key may be out.
	if err := tokens.EnsureOwnKey(); err != nil {
		t.Fatal(err)
	}
	again, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(again, data) {
		t.Errorf("the key file changed on the second start: %v", err)
	}
	if left := dirNames(t, dir); len(left) != 1 {
		t.Errorf("temporary files left behind: %v", left)
	}
}

func TestEnsureOwnKeyFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup func(t *testing.T, dir string)
		want  string
	}{
		{"invalid file is not replaced", func(t *testing.T, dir string) {
			writeFile(t, dir, testNode+".yaml", "keys: []\n")
		}, "no keys"},
		{"unreadable file", func(t *testing.T, dir string) {
			if err := os.Mkdir(filepath.Join(dir, testNode+".yaml"), 0o700); err != nil {
				t.Fatal(err)
			}
		}, "read signing key file"},
		{"folder is missing", func(t *testing.T, dir string) {
			if err := os.Remove(dir); err != nil {
				t.Fatal(err)
			}
		}, "create signing key file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			tt.setup(t, dir)
			tokens := NewTokens(dir, testNode, time.Minute, newFakeClock().Now, discard)
			err := tokens.EnsureOwnKey()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestEnsureOwnKeyRandomFailure(t *testing.T) {
	t.Parallel()
	tokens := NewTokens(t.TempDir(), testNode, time.Minute, newFakeClock().Now, discard)
	tokens.random = strings.NewReader("short")
	if err := tokens.EnsureOwnKey(); err == nil {
		t.Error("want error")
	}
}

func TestTokensIgnoreForeignFiles(t *testing.T) {
	t.Parallel()
	log, buf := logged()
	dir := t.TempDir()
	tokens := NewTokens(dir, testNode, time.Minute, newFakeClock().Now, log)
	writeFile(t, dir, testNode+".yaml", keysYAML([2]string{"own", b64(secretA)}))
	foreign := keysYAML([2]string{"evil", b64(secretB)})
	for _, name := range []string{
		".tmp-123.yaml", ".fedcba9876543210.yaml", "fedcba9876543210.yaml.tmp",
		"fedcba9876543210.yaml~", "notes.txt", "fedcba9876543210",
	} {
		writeFile(t, dir, name, foreign)
	}
	if err := os.Mkdir(filepath.Join(dir, otherNode+".yaml"), 0o700); err != nil {
		t.Fatal(err)
	}
	evil := forge(secretB, jsonSegment(t, obj{"alg": "HS256", "kid": "evil"}), jsonSegment(t, obj{}))
	if _, err := tokens.Verify(evil); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("key of a foreign file accepted: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("files that are not key files must be ignored silently:\n%s", buf)
	}

	// A .yaml file whose name is not a node id is ignored, and the log says so.
	writeFile(t, dir, "backup.yaml", foreign)
	if _, err := tokens.Verify(evil); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("key of a badly named file accepted: %v", err)
	}
	if !strings.Contains(buf.String(), "backup.yaml") || strings.Contains(buf.String(), b64(secretB)) {
		t.Errorf("log = %s", buf)
	}
}

func TestTokensDuplicateKeyID(t *testing.T) {
	t.Parallel()
	log, buf := logged()
	dir := t.TempDir()
	// otherNode sorts after testNode, so testNode's key keeps the id.
	writeFile(t, dir, testNode+".yaml", keysYAML([2]string{"shared", b64(secretA)}))
	writeFile(t, dir, otherNode+".yaml", keysYAML([2]string{"shared", b64(secretB)}, [2]string{"only", b64(secretB)}))
	tokens := NewTokens(dir, otherNode, time.Minute, newFakeClock().Now, log)
	header := jsonSegment(t, obj{"alg": "HS256", "kid": "shared"})
	payload := jsonSegment(t, obj{"sub": "a", "shares": []string{}, "iat": newFakeClock().Now().Unix(), "exp": newFakeClock().Now().Unix() + 60})
	if _, err := tokens.Verify(forge(secretA, header, payload)); err != nil {
		t.Errorf("the first file's key: %v", err)
	}
	if _, err := tokens.Verify(forge(secretB, header, payload)); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("the later file's key must lose: %v", err)
	}
	onlyHeader := jsonSegment(t, obj{"alg": "HS256", "kid": "only"})
	if _, err := tokens.Verify(forge(secretB, onlyHeader, payload)); err != nil {
		t.Errorf("a unique key of the later file: %v", err)
	}
	for _, want := range []string{testNode + ".yaml", otherNode + ".yaml"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("warning does not name %q:\n%s", want, buf)
		}
	}
	// The id stays out of the log: a mistake in a file may put a secret there.
	if strings.Contains(buf.String(), "shared") {
		t.Errorf("warning repeats the key id:\n%s", buf)
	}
	if strings.Count(buf.String(), "duplicate key id") != 1 {
		t.Errorf("the warning must be logged once per change:\n%s", buf)
	}
}

func TestTokensOtherFileBreaksAndRecovers(t *testing.T) {
	t.Parallel()
	log, buf := logged()
	dir := t.TempDir()
	writeFile(t, dir, testNode+".yaml", keysYAML([2]string{"own", b64(secretA)}))
	tokens := NewTokens(dir, testNode, time.Minute, newFakeClock().Now, log)
	other := filepath.Join(dir, otherNode+".yaml")
	if _, _, err := tokens.Issue("alice", nil); err != nil {
		t.Fatal(err)
	}
	header := jsonSegment(t, obj{"alg": "HS256", "kid": "theirs"})
	now := newFakeClock().Now().Unix()
	token := forge(secretB, header, jsonSegment(t, obj{"sub": "x", "shares": []string{}, "iat": now, "exp": now + 60}))

	// A file that is half written when the sync tool copies it is ignored.
	writeFile(t, dir, otherNode+".yaml", "keys:\n  - id: \"theirs\"\n  ")
	if _, err := tokens.Verify(token); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("garbage accepted: %v", err)
	}
	if _, _, err := tokens.Issue("alice", nil); err != nil {
		t.Errorf("a bad foreign file stopped signing: %v", err)
	}
	if !strings.Contains(buf.String(), otherNode+".yaml") {
		t.Errorf("no warning:\n%s", buf)
	}

	// Once complete it is picked up without a restart.
	writeFile(t, dir, otherNode+".yaml", keysYAML([2]string{"theirs", b64(secretB)}))
	if _, err := tokens.Verify(token); err != nil {
		t.Errorf("valid file not picked up: %v", err)
	}
	// Its key stays in use while the file is bad, and goes when the file is removed.
	writeFile(t, dir, otherNode+".yaml", "garbage: [")
	if _, err := tokens.Verify(token); err != nil {
		t.Errorf("last good content not kept: %v", err)
	}
	if err := os.Remove(other); err != nil {
		t.Fatal(err)
	}
	if _, err := tokens.Verify(token); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("key of a removed file still accepted: %v", err)
	}
}

func TestTokensSignWithFirstKeyOfOwnFileOnly(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, otherNode+".yaml", keysYAML([2]string{"a-first", b64(secretB)}))
	writeFile(t, dir, testNode+".yaml", twoKeys())
	tokens := NewTokens(dir, testNode, time.Minute, newFakeClock().Now, discard)
	if header := decodeHeader(t, mustIssue(t, tokens)); header.Kid != "new" {
		t.Errorf("kid = %q, want the first key of this node's own file", header.Kid)
	}
}
