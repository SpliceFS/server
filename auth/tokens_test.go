package auth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
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

// newTestTokens writes a keys file and returns Tokens that use it.
func newTestTokens(t *testing.T, content string, clock *fakeClock) (*Tokens, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "keys.yaml")
	writeFile(t, filepath.Dir(path), "keys.yaml", content)
	return NewTokens(path, 30*time.Minute, clock.Now), path
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
			wellFormed := forge(secretA, jsonSegment(t, obj{"alg": "HS256", "kid": "new"}), jsonSegment(t, obj{}))
			if _, err := tokens.Verify(wellFormed); err == nil || errors.Is(err, ErrInvalidToken) {
				t.Errorf("Verify: err = %v, want a keys error", err)
			}
		})
	}
}

func TestTokensKeysFileMissing(t *testing.T) {
	t.Parallel()
	tokens := NewTokens(filepath.Join(t.TempDir(), "absent.yaml"), time.Minute, newFakeClock().Now)
	if _, _, err := tokens.Issue("alice", nil); err == nil {
		t.Error("want error")
	}
}

func TestTokensKeysFileUnreadable(t *testing.T) {
	t.Parallel()
	// A directory can be stat'ed but not read.
	tokens := NewTokens(t.TempDir(), time.Minute, newFakeClock().Now)
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
	writeFile(t, filepath.Dir(path), "keys.yaml", replacement)
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
	writeFile(t, filepath.Dir(path), "keys.yaml", keysYAML([2]string{"three", b64(secretB)}, [2]string{"two", b64(secretA)}))
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

func TestTokensFailedReloadReturnsError(t *testing.T) {
	t.Parallel()
	tokens, path := newTestTokens(t, keysYAML([2]string{"one", b64(secretA)}), newFakeClock())
	token, _, err := tokens.Issue("alice", nil)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Dir(path), "keys.yaml", "keys: [broken\n")
	if _, err := tokens.Verify(token); err == nil || errors.Is(err, ErrInvalidToken) {
		t.Errorf("Verify after bad reload: err = %v, want a keys error", err)
	}
	if _, _, err := tokens.Issue("alice", nil); err == nil {
		t.Error("Issue after bad reload: want error")
	}
	writeFile(t, filepath.Dir(path), "keys.yaml", keysYAML([2]string{"one", b64(secretA)}))
	if _, err := tokens.Verify(token); err != nil {
		t.Errorf("after the file is fixed: %v", err)
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
