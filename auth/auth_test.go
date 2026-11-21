package auth

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SpliceFS/common/api"
)

// verifyCall records one call of the injected password verifier.
type verifyCall struct{ encoded, password string }

// authFixture is an Authenticator over temporary files with a recording verifier.
type authFixture struct {
	auth     *Authenticator
	usersDir string
	clock    *fakeClock
	calls    []verifyCall
	result   bool
	err      error
}

func newAuthFixture(t *testing.T, limit int) *authFixture {
	t.Helper()
	fixture := &authFixture{usersDir: t.TempDir(), clock: newFakeClock(), result: true}
	writeUser(t, fixture.usersDir, aliceID, "name: alice\npassword_hash: alice-hash\nshares: [media]\n")
	writeUser(t, fixture.usersDir, daveID, "name: dave\npassword_hash: dave-hash\nshares: [media]\ndisabled: true\n")
	fixture.auth = New(
		newUsersAt(fixture.usersDir, fixture.clock),
		newTestTokens(stubNodes{}, fixture.clock),
		NewLimiter(limit, time.Minute, fixture.clock.Now),
		4,
	)
	fixture.auth.verify = func(encoded, password string) (bool, error) {
		fixture.calls = append(fixture.calls, verifyCall{encoded, password})
		return fixture.result, fixture.err
	}
	return fixture
}

// wantLoginError logs user in from a fixed address and checks that the error is want, or
// nil for a login that succeeds.
func (fixture *authFixture) wantLoginError(t *testing.T, user string, want error) {
	t.Helper()
	if _, _, err := fixture.auth.Login(context.Background(), "192.0.2.1", user, "pw"); !errors.Is(err, want) {
		t.Errorf("login of %q: err = %v, want %v", user, err, want)
	}
}

func TestLoginSuccess(t *testing.T) {
	t.Parallel()
	fixture := newAuthFixture(t, 10)
	token, expires, err := fixture.auth.Login(context.Background(), "192.0.2.1", "alice", "pw")
	if err != nil {
		t.Fatal(err)
	}
	if want := fixture.clock.Now().Add(30 * time.Minute); !expires.Equal(want) {
		t.Errorf("expires = %v, want %v", expires, want)
	}
	if len(fixture.calls) != 1 || fixture.calls[0] != (verifyCall{"alice-hash", "pw"}) {
		t.Errorf("verify calls = %v", fixture.calls)
	}
	claims, err := fixture.auth.Verify(token)
	if err != nil {
		t.Fatal(err)
	}
	if claims.User != "alice" || !claims.Allows("media") || claims.Allows("docs") || !claims.Expires.Equal(expires) {
		t.Errorf("claims = %+v, want alice, the share media and the expiry %v", claims, expires)
	}
}

// The token names the user by id and carries no shares: what the user may do is read from the
// state of the user.
func TestLoginTokenCarriesTheUserIDAndNoShares(t *testing.T) {
	t.Parallel()
	fixture := newAuthFixture(t, 10)
	token, _, err := fixture.auth.Login(context.Background(), "192.0.2.1", "alice", "pw")
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := decodeSegment(strings.Split(token, ".")[1], &payload); err != nil {
		t.Fatal(err)
	}
	if payload["sub"] != aliceID || len(payload) != 3 || payload["iat"] == nil || payload["exp"] == nil {
		t.Errorf("payload = %v, want sub, iat and exp, with the id of alice as sub", payload)
	}
}

// loginAlice logs alice in and returns her token.
func (fixture *authFixture) loginAlice(t *testing.T) string {
	t.Helper()
	token, _, err := fixture.auth.Login(context.Background(), "192.0.2.1", "alice", "pw")
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func TestVerifyTakesTheSharesFromTheStateOfTheUser(t *testing.T) {
	t.Parallel()
	fixture := newAuthFixture(t, 10)
	token := fixture.loginAlice(t)
	writeUser(t, fixture.usersDir, aliceID, "name: alice\npassword_hash: alice-hash\nshares: [docs, music]\n")

	fixture.clock.Advance(RecheckInterval - time.Second)
	if claims, err := fixture.auth.Verify(token); err != nil || !claims.Allows("media") {
		t.Errorf("inside the interval: %+v, %v, want the shares as they were", claims, err)
	}
	fixture.clock.Advance(time.Second)
	claims, err := fixture.auth.Verify(token)
	if err != nil {
		t.Fatal(err)
	}
	if claims.Allows("media") || !claims.Allows("docs") || !claims.Allows("music") {
		t.Errorf("after the interval: shares of %+v, want docs and music", claims)
	}
}

func TestVerifyRefusesTheTokenOfAUserWhoIsGoneOrDisabled(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		change func(t *testing.T, fixture *authFixture)
	}{
		{"disabled", func(t *testing.T, fixture *authFixture) {
			t.Helper()
			writeUser(t, fixture.usersDir, aliceID, "name: alice\npassword_hash: h\ndisabled: true\n")
		}},
		{"removed", func(t *testing.T, fixture *authFixture) {
			t.Helper()
			if err := os.Remove(filepath.Join(fixture.usersDir, aliceID+".yaml")); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fixture := newAuthFixture(t, 10)
			token := fixture.loginAlice(t)
			tt.change(t, fixture)
			if _, err := fixture.auth.Verify(token); err != nil {
				t.Fatalf("inside the interval: %v, want the token accepted until the next check", err)
			}
			fixture.clock.Advance(RecheckInterval)
			claims, err := fixture.auth.Verify(token)
			if !errors.Is(err, api.ErrInvalidToken) || !reflect.DeepEqual(claims, api.Claims{}) {
				t.Errorf("after the interval: %+v, %v, want api.ErrInvalidToken and no claims", claims, err)
			}
		})
	}
}

// A token is only as good as the user it names: one signed by this node for an id without a
// file, or for a name, as earlier versions did, is no login.
func TestVerifyRefusesTokensOfUnknownUsers(t *testing.T) {
	t.Parallel()
	fixture := newAuthFixture(t, 10)
	for _, sub := range []string{carolID, "alice", "../alice"} {
		token, _, err := fixture.auth.tokens.Issue(sub)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.auth.Verify(token); !errors.Is(err, api.ErrInvalidToken) {
			t.Errorf("sub %q: err = %v, want api.ErrInvalidToken", sub, err)
		}
	}
}

func TestVerifyChecksTheTokenBeforeTheUser(t *testing.T) {
	t.Parallel()
	fixture := newAuthFixture(t, 10)
	token := fixture.loginAlice(t)
	fixture.clock.Advance(time.Hour)
	fixture.auth.users.stat = func(string) (fs.FileInfo, error) {
		t.Error("the file of a user was checked for a token that has expired")
		return nil, errors.New("must not be called")
	}
	if _, err := fixture.auth.Verify(token); !errors.Is(err, api.ErrExpiredToken) {
		t.Errorf("err = %v, want api.ErrExpiredToken", err)
	}
}

// When the file of the user cannot be checked the answer is an error of the server, not a 401
// the client would answer with a new login, and never an accepted token.
func TestVerifyFailsClosedWhenTheUserCannotBeChecked(t *testing.T) {
	t.Parallel()
	fixture := newAuthFixture(t, 10)
	token := fixture.loginAlice(t)
	fixture.clock.Advance(RecheckInterval)
	cause := &fs.PathError{Op: "stat", Path: aliceID, Err: fs.ErrPermission}
	fixture.auth.users.stat = func(string) (fs.FileInfo, error) { return nil, cause }
	claims, err := fixture.auth.Verify(token)
	if !errors.Is(err, cause) || errors.Is(err, api.ErrInvalidToken) || errors.Is(err, api.ErrExpiredToken) {
		t.Errorf("err = %v, want the cause and neither of the token errors", err)
	}
	if !reflect.DeepEqual(claims, api.Claims{}) {
		t.Errorf("claims = %+v, want none", claims)
	}
}

func TestLoginBadCredentials(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		user     string
		result   bool
		wantHash string
	}{
		{"wrong password", "alice", false, "alice-hash"},
		{"unknown user", "nobody", true, dummyHash},
		{"invalid user name", "../alice", true, dummyHash},
		{"disabled user with right password", "dave", true, dummyHash},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fixture := newAuthFixture(t, 10)
			fixture.result = tt.result
			token, expires, err := fixture.auth.Login(context.Background(), "192.0.2.1", tt.user, "pw")
			if !errors.Is(err, api.ErrBadCredentials) {
				t.Fatalf("err = %v, want api.ErrBadCredentials", err)
			}
			if token != "" || !expires.IsZero() {
				t.Errorf("token %q and expires %v must be empty", token, expires)
			}
			// Every failure costs exactly one password verification.
			if len(fixture.calls) != 1 || fixture.calls[0] != (verifyCall{tt.wantHash, "pw"}) {
				t.Errorf("verify calls = %v, want one with %q", fixture.calls, tt.wantHash)
			}
		})
	}
}

func TestLoginUnknownUserIgnoresVerifierFailure(t *testing.T) {
	t.Parallel()
	fixture := newAuthFixture(t, 10)
	fixture.err = errors.New("verifier broke")
	_, _, err := fixture.auth.Login(context.Background(), "192.0.2.1", "nobody", "pw")
	if !errors.Is(err, api.ErrBadCredentials) {
		t.Errorf("err = %v, want api.ErrBadCredentials", err)
	}
}

func TestLoginRateLimited(t *testing.T) {
	t.Parallel()
	fixture := newAuthFixture(t, 2)
	for range 2 {
		if _, _, err := fixture.auth.Login(context.Background(), "192.0.2.1", "alice", "bad"); err != nil {
			t.Fatal(err)
		}
	}
	fixture.calls = nil
	token, _, err := fixture.auth.Login(context.Background(), "192.0.2.1", "alice", "pw")
	if !errors.Is(err, api.ErrRateLimited) || token != "" {
		t.Fatalf("token %q, err = %v, want api.ErrRateLimited", token, err)
	}
	if len(fixture.calls) != 0 {
		t.Errorf("a rate limited attempt must not verify a password: %v", fixture.calls)
	}
	if _, _, err := fixture.auth.Login(context.Background(), "192.0.2.2", "alice", "pw"); err != nil {
		t.Errorf("other address: %v", err)
	}
}

func TestLoginCountsEveryAttempt(t *testing.T) {
	t.Parallel()
	fixture := newAuthFixture(t, 2)
	fixture.result = false
	fixture.wantLoginError(t, "nobody", api.ErrBadCredentials)
	fixture.wantLoginError(t, "dave", api.ErrBadCredentials)
	_, _, err := fixture.auth.Login(context.Background(), "192.0.2.1", "alice", "pw")
	if !errors.Is(err, api.ErrRateLimited) {
		t.Errorf("err = %v, want api.ErrRateLimited", err)
	}
}

func TestLoginBrokenUserFileIsNoUser(t *testing.T) {
	t.Parallel()
	fixture := newAuthFixture(t, 10)
	writeUser(t, fixture.usersDir, carolID, "name: carol\npassword_hash: [\n")
	fixture.wantLoginError(t, "carol", api.ErrBadCredentials)
	if len(fixture.calls) != 1 || fixture.calls[0].encoded != dummyHash {
		t.Errorf("verify calls = %v, want one against the dummy hash", fixture.calls)
	}
}

func TestLoginOtherFailuresAreNotBadCredentials(t *testing.T) {
	t.Parallel()
	t.Run("verifier error", func(t *testing.T) {
		t.Parallel()
		fixture := newAuthFixture(t, 10)
		fixture.err = errors.New("invalid password hash")
		_, _, err := fixture.auth.Login(context.Background(), "192.0.2.1", "alice", "pw-secret")
		if err == nil || errors.Is(err, api.ErrBadCredentials) {
			t.Fatalf("err = %v", err)
		}
		if !errors.Is(err, fixture.err) || strings.Contains(err.Error(), "pw-secret") {
			t.Errorf("err = %v, want it to wrap the cause and hide the password", err)
		}
	})
	t.Run("token cannot be issued", func(t *testing.T) {
		t.Parallel()
		fixture := newAuthFixture(t, 10)
		fixture.auth.tokens.marshal = func(any) ([]byte, error) { return nil, errors.New("marshal failed") }
		_, _, err := fixture.auth.Login(context.Background(), "192.0.2.1", "alice", "pw")
		if err == nil || errors.Is(err, api.ErrBadCredentials) {
			t.Errorf("err = %v", err)
		}
	})
}

func TestAuthenticatorVerify(t *testing.T) {
	t.Parallel()
	fixture := newAuthFixture(t, 10)
	if _, err := fixture.auth.Verify("not-a-token"); !errors.Is(err, api.ErrInvalidToken) {
		t.Errorf("err = %v, want api.ErrInvalidToken", err)
	}
	token, _, err := fixture.auth.Login(context.Background(), "192.0.2.1", "alice", "pw")
	if err != nil {
		t.Fatal(err)
	}
	fixture.clock.Advance(time.Hour)
	if _, err := fixture.auth.Verify(token); !errors.Is(err, api.ErrExpiredToken) {
		t.Errorf("err = %v, want api.ErrExpiredToken", err)
	}
}

func TestNewUsesRealVerifier(t *testing.T) {
	t.Parallel()
	a := New(newTestUsers(t.TempDir()), nil, nil, 1)
	ok, err := a.verify(smallHash("pw"), "pw")
	if err != nil || !ok {
		t.Errorf("default verifier = %v, %v; want true, nil", ok, err)
	}
}

func TestLoginWithRealPasswordHash(t *testing.T) {
	t.Parallel()
	fixture := newAuthFixture(t, 10)
	fixture.auth.verify = VerifyPassword
	writeUser(t, fixture.usersDir, carolID,
		"name: frank\npassword_hash: \""+smallHash("right")+"\"\nshares: [\"*\"]\n")
	if _, _, err := fixture.auth.Login(context.Background(), "192.0.2.1", "frank", "right"); err != nil {
		t.Errorf("right password: %v", err)
	}
	_, _, err := fixture.auth.Login(context.Background(), "192.0.2.1", "frank", "wrong")
	if !errors.Is(err, api.ErrBadCredentials) {
		t.Errorf("wrong password: err = %v", err)
	}
}

func TestDummyHashHasRealParameters(t *testing.T) {
	t.Parallel()
	got, err := parseHash(dummyHash)
	if err != nil {
		t.Fatal(err)
	}
	want := productionCost()
	if got.memory != want.memory || got.time != want.time || got.threads != want.threads || len(got.key) != keyLength {
		t.Errorf("dummy hash parameters = %+v", got)
	}
}

func TestNewSlotCount(t *testing.T) {
	t.Parallel()
	tests := []struct{ slots, want int }{{-3, 1}, {0, 1}, {1, 1}, {4, 4}}
	for _, tt := range tests {
		if got := cap(New(nil, nil, nil, tt.slots).slots); got != tt.want {
			t.Errorf("New(slots=%d): capacity %d, want %d", tt.slots, got, tt.want)
		}
	}
}

func TestLoginWaitsForSlotUntilContextEnds(t *testing.T) {
	t.Parallel()
	fixture := newAuthFixture(t, 10)
	fixture.auth.slots = make(chan struct{}, 1)
	fixture.auth.slots <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	token, expires, err := fixture.auth.Login(ctx, "192.0.2.1", "alice", "pw")
	if !errors.Is(err, context.Canceled) || errors.Is(err, api.ErrBadCredentials) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if token != "" || !expires.IsZero() || len(fixture.calls) != 0 {
		t.Errorf("token %q, expires %v, verify calls %v; want none", token, expires, fixture.calls)
	}
}

func TestLoginReleasesSlot(t *testing.T) {
	t.Parallel()
	fixture := newAuthFixture(t, 10)
	fixture.auth.slots = make(chan struct{}, 1)
	for _, attempt := range []struct {
		user string
		want error
	}{{"alice", nil}, {"nobody", api.ErrBadCredentials}, {"alice", nil}} {
		fixture.result = attempt.user == "alice"
		fixture.wantLoginError(t, attempt.user, attempt.want)
		if n := len(fixture.auth.slots); n != 0 {
			t.Fatalf("after login of %q: %d slots still held", attempt.user, n)
		}
	}
}

func TestLoginBoundsConcurrentVerifications(t *testing.T) {
	t.Parallel()
	fixture := newAuthFixture(t, 100)
	const slots, logins = 2, 8
	fixture.auth.slots = make(chan struct{}, slots)
	var mu sync.Mutex
	var running, peak int
	entered := make(chan struct{}, logins)
	release := make(chan struct{})
	fixture.auth.verify = func(string, string) (bool, error) {
		mu.Lock()
		running++
		peak = max(peak, running)
		mu.Unlock()
		entered <- struct{}{}
		<-release
		mu.Lock()
		running--
		mu.Unlock()
		return true, nil
	}
	var wg sync.WaitGroup
	for range logins {
		wg.Go(func() {
			fixture.wantLoginError(t, "alice", nil)
		})
	}
	for range slots {
		<-entered
	}
	for range logins {
		release <- struct{}{}
	}
	wg.Wait()
	if peak != slots {
		t.Errorf("peak concurrent verifications = %d, want %d", peak, slots)
	}
}
