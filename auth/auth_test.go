package auth

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// verifyCall records one call of the injected password verifier.
type verifyCall struct{ encoded, password string }

// authFixture is an Authenticator over temporary files with a recording verifier.
type authFixture struct {
	auth     *Authenticator
	usersDir string
	keysPath string
	clock    *fakeClock
	calls    []verifyCall
	result   bool
	err      error
}

func newAuthFixture(t *testing.T, limit int) *authFixture {
	t.Helper()
	fixture := &authFixture{usersDir: t.TempDir(), clock: newFakeClock(), result: true}
	keysDir := t.TempDir()
	fixture.keysPath = filepath.Join(keysDir, testNode+".yaml")
	writeFile(t, keysDir, testNode+".yaml", twoKeys())
	writeFile(t, fixture.usersDir, "alice.yaml", "password_hash: alice-hash\nshares: [media]\n")
	writeFile(t, fixture.usersDir, "dave.yaml", "password_hash: dave-hash\nshares: [media]\ndisabled: true\n")
	fixture.auth = New(
		NewUsers(fixture.usersDir),
		NewTokens(keysDir, testNode, 30*time.Minute, fixture.clock.Now, discard()),
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
	if claims.User != "alice" || !claims.Allows("media") || claims.Allows("docs") {
		t.Errorf("claims = %+v", claims)
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
			if !errors.Is(err, ErrBadCredentials) {
				t.Fatalf("err = %v, want ErrBadCredentials", err)
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
	if !errors.Is(err, ErrBadCredentials) {
		t.Errorf("err = %v, want ErrBadCredentials", err)
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
	if !errors.Is(err, ErrRateLimited) || token != "" {
		t.Fatalf("token %q, err = %v, want ErrRateLimited", token, err)
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
	fixture.wantLoginError(t, "nobody", ErrBadCredentials)
	fixture.wantLoginError(t, "dave", ErrBadCredentials)
	if _, _, err := fixture.auth.Login(context.Background(), "192.0.2.1", "alice", "pw"); !errors.Is(err, ErrRateLimited) {
		t.Errorf("err = %v, want ErrRateLimited", err)
	}
}

func TestLoginOtherFailuresAreNotBadCredentials(t *testing.T) {
	t.Parallel()
	t.Run("broken user file", func(t *testing.T) {
		t.Parallel()
		fixture := newAuthFixture(t, 10)
		writeFile(t, fixture.usersDir, "erin.yaml", "password_hash: [\n")
		_, _, err := fixture.auth.Login(context.Background(), "192.0.2.1", "erin", "pw")
		if err == nil || errors.Is(err, ErrBadCredentials) || errors.Is(err, ErrNoUser) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("verifier error", func(t *testing.T) {
		t.Parallel()
		fixture := newAuthFixture(t, 10)
		fixture.err = errors.New("invalid password hash")
		_, _, err := fixture.auth.Login(context.Background(), "192.0.2.1", "alice", "pw-secret")
		if err == nil || errors.Is(err, ErrBadCredentials) {
			t.Fatalf("err = %v", err)
		}
		if !errors.Is(err, fixture.err) || strings.Contains(err.Error(), "pw-secret") {
			t.Errorf("err = %v, want it to wrap the cause and hide the password", err)
		}
	})
	t.Run("broken keys file", func(t *testing.T) {
		t.Parallel()
		fixture := newAuthFixture(t, 10)
		writeFile(t, filepath.Dir(fixture.keysPath), testNode+".yaml", "keys: []\n")
		_, _, err := fixture.auth.Login(context.Background(), "192.0.2.1", "alice", "pw")
		if err == nil || errors.Is(err, ErrBadCredentials) {
			t.Errorf("err = %v", err)
		}
	})
}

func TestAuthenticatorVerify(t *testing.T) {
	t.Parallel()
	fixture := newAuthFixture(t, 10)
	if _, err := fixture.auth.Verify("not-a-token"); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("err = %v, want ErrInvalidToken", err)
	}
	token, _, err := fixture.auth.Login(context.Background(), "192.0.2.1", "alice", "pw")
	if err != nil {
		t.Fatal(err)
	}
	fixture.clock.Advance(time.Hour)
	if _, err := fixture.auth.Verify(token); !errors.Is(err, ErrExpiredToken) {
		t.Errorf("err = %v, want ErrExpiredToken", err)
	}
}

func TestNewUsesRealVerifier(t *testing.T) {
	t.Parallel()
	a := New(NewUsers(t.TempDir()), nil, nil, 1)
	ok, err := a.verify(smallHash("pw"), "pw")
	if err != nil || !ok {
		t.Errorf("default verifier = %v, %v; want true, nil", ok, err)
	}
}

func TestLoginWithRealPasswordHash(t *testing.T) {
	t.Parallel()
	fixture := newAuthFixture(t, 10)
	fixture.auth.verify = VerifyPassword
	writeFile(t, fixture.usersDir, "frank.yaml", "password_hash: \""+smallHash("right")+"\"\nshares: [\"*\"]\n")
	if _, _, err := fixture.auth.Login(context.Background(), "192.0.2.1", "frank", "right"); err != nil {
		t.Errorf("right password: %v", err)
	}
	_, _, err := fixture.auth.Login(context.Background(), "192.0.2.1", "frank", "wrong")
	if !errors.Is(err, ErrBadCredentials) {
		t.Errorf("wrong password: err = %v", err)
	}
}

func TestDummyHashHasRealParameters(t *testing.T) {
	t.Parallel()
	h, err := parseHash(dummyHash)
	if err != nil {
		t.Fatal(err)
	}
	if h.memory != hashMemory || h.time != hashTime || h.threads != hashThreads || len(h.key) != keyLength {
		t.Errorf("dummy hash parameters = %+v", h)
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
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrBadCredentials) {
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
	}{{"alice", nil}, {"nobody", ErrBadCredentials}, {"alice", nil}} {
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
