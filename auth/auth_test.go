package auth

import (
	"context"
	"errors"
	"os"
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
	f := &authFixture{usersDir: t.TempDir(), clock: newFakeClock(), result: true}
	f.keysPath = filepath.Join(t.TempDir(), "keys.yaml")
	if err := os.WriteFile(f.keysPath, []byte(twoKeys()), 0o600); err != nil {
		t.Fatal(err)
	}
	writeFile(t, f.usersDir, "alice.yaml", "password_hash: alice-hash\nshares: [media]\n")
	writeFile(t, f.usersDir, "dave.yaml", "password_hash: dave-hash\nshares: [media]\ndisabled: true\n")
	f.auth = New(
		NewUsers(f.usersDir),
		NewTokens(f.keysPath, 30*time.Minute, f.clock.Now),
		NewLimiter(limit, time.Minute, f.clock.Now),
		4,
	)
	f.auth.verify = func(encoded, password string) (bool, error) {
		f.calls = append(f.calls, verifyCall{encoded, password})
		return f.result, f.err
	}
	return f
}

func TestLoginSuccess(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, 10)
	token, expires, err := f.auth.Login(context.Background(), "192.0.2.1", "alice", "pw")
	if err != nil {
		t.Fatal(err)
	}
	if want := f.clock.Now().Add(30 * time.Minute); !expires.Equal(want) {
		t.Errorf("expires = %v, want %v", expires, want)
	}
	if len(f.calls) != 1 || f.calls[0] != (verifyCall{"alice-hash", "pw"}) {
		t.Errorf("verify calls = %v", f.calls)
	}
	claims, err := f.auth.Verify(token)
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
			f := newAuthFixture(t, 10)
			f.result = tt.result
			token, expires, err := f.auth.Login(context.Background(), "192.0.2.1", tt.user, "pw")
			if !errors.Is(err, ErrBadCredentials) {
				t.Fatalf("err = %v, want ErrBadCredentials", err)
			}
			if token != "" || !expires.IsZero() {
				t.Errorf("token %q and expires %v must be empty", token, expires)
			}
			// Every failure costs exactly one password verification.
			if len(f.calls) != 1 || f.calls[0] != (verifyCall{tt.wantHash, "pw"}) {
				t.Errorf("verify calls = %v, want one with %q", f.calls, tt.wantHash)
			}
		})
	}
}

func TestLoginUnknownUserIgnoresVerifierFailure(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, 10)
	f.err = errors.New("verifier broke")
	if _, _, err := f.auth.Login(context.Background(), "192.0.2.1", "nobody", "pw"); !errors.Is(err, ErrBadCredentials) {
		t.Errorf("err = %v, want ErrBadCredentials", err)
	}
}

func TestLoginRateLimited(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, 2)
	for range 2 {
		if _, _, err := f.auth.Login(context.Background(), "192.0.2.1", "alice", "bad"); err != nil {
			t.Fatal(err)
		}
	}
	f.calls = nil
	token, _, err := f.auth.Login(context.Background(), "192.0.2.1", "alice", "pw")
	if !errors.Is(err, ErrRateLimited) || token != "" {
		t.Fatalf("token %q, err = %v, want ErrRateLimited", token, err)
	}
	if len(f.calls) != 0 {
		t.Errorf("a rate limited attempt must not verify a password: %v", f.calls)
	}
	if _, _, err := f.auth.Login(context.Background(), "192.0.2.2", "alice", "pw"); err != nil {
		t.Errorf("other address: %v", err)
	}
}

func TestLoginCountsEveryAttempt(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, 2)
	f.result = false
	_, _, _ = f.auth.Login(context.Background(), "192.0.2.1", "nobody", "pw")
	_, _, _ = f.auth.Login(context.Background(), "192.0.2.1", "dave", "pw")
	if _, _, err := f.auth.Login(context.Background(), "192.0.2.1", "alice", "pw"); !errors.Is(err, ErrRateLimited) {
		t.Errorf("err = %v, want ErrRateLimited", err)
	}
}

func TestLoginOtherFailuresAreNotBadCredentials(t *testing.T) {
	t.Parallel()
	t.Run("broken user file", func(t *testing.T) {
		t.Parallel()
		f := newAuthFixture(t, 10)
		writeFile(t, f.usersDir, "erin.yaml", "password_hash: [\n")
		_, _, err := f.auth.Login(context.Background(), "192.0.2.1", "erin", "pw")
		if err == nil || errors.Is(err, ErrBadCredentials) || errors.Is(err, ErrNoUser) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("verifier error", func(t *testing.T) {
		t.Parallel()
		f := newAuthFixture(t, 10)
		f.err = errors.New("invalid password hash")
		_, _, err := f.auth.Login(context.Background(), "192.0.2.1", "alice", "pw-secret")
		if err == nil || errors.Is(err, ErrBadCredentials) {
			t.Fatalf("err = %v", err)
		}
		if !errors.Is(err, f.err) || strings.Contains(err.Error(), "pw-secret") {
			t.Errorf("err = %v, want it to wrap the cause and hide the password", err)
		}
	})
	t.Run("broken keys file", func(t *testing.T) {
		t.Parallel()
		f := newAuthFixture(t, 10)
		writeFile(t, filepath.Dir(f.keysPath), "keys.yaml", "keys: []\n")
		_, _, err := f.auth.Login(context.Background(), "192.0.2.1", "alice", "pw")
		if err == nil || errors.Is(err, ErrBadCredentials) {
			t.Errorf("err = %v", err)
		}
	})
}

func TestAuthenticatorVerify(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, 10)
	if _, err := f.auth.Verify("not-a-token"); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("err = %v, want ErrInvalidToken", err)
	}
	token, _, err := f.auth.Login(context.Background(), "192.0.2.1", "alice", "pw")
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(time.Hour)
	if _, err := f.auth.Verify(token); !errors.Is(err, ErrExpiredToken) {
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
	f := newAuthFixture(t, 10)
	f.auth.verify = VerifyPassword
	writeFile(t, f.usersDir, "frank.yaml", "password_hash: \""+smallHash("right")+"\"\nshares: [\"*\"]\n")
	if _, _, err := f.auth.Login(context.Background(), "192.0.2.1", "frank", "right"); err != nil {
		t.Errorf("right password: %v", err)
	}
	if _, _, err := f.auth.Login(context.Background(), "192.0.2.1", "frank", "wrong"); !errors.Is(err, ErrBadCredentials) {
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
	f := newAuthFixture(t, 10)
	f.auth.slots = make(chan struct{}, 1)
	f.auth.slots <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	token, expires, err := f.auth.Login(ctx, "192.0.2.1", "alice", "pw")
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrBadCredentials) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if token != "" || !expires.IsZero() || len(f.calls) != 0 {
		t.Errorf("token %q, expires %v, verify calls %v; want none", token, expires, f.calls)
	}
}

func TestLoginReleasesSlot(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, 10)
	f.auth.slots = make(chan struct{}, 1)
	for _, user := range []string{"alice", "nobody", "alice"} {
		f.result = user == "alice"
		_, _, _ = f.auth.Login(context.Background(), "192.0.2.1", user, "pw")
		if n := len(f.auth.slots); n != 0 {
			t.Fatalf("after login of %q: %d slots still held", user, n)
		}
	}
}

func TestLoginBoundsConcurrentVerifications(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, 100)
	const slots, logins = 2, 8
	f.auth.slots = make(chan struct{}, slots)
	var mu sync.Mutex
	var running, peak int
	entered := make(chan struct{}, logins)
	release := make(chan struct{})
	f.auth.verify = func(string, string) (bool, error) {
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
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _ = f.auth.Login(context.Background(), "192.0.2.1", "alice", "pw")
		}()
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
