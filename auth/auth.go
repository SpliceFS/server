// Package auth implements logins for the SpliceFS server: argon2id password hashes, user
// files, tokens signed with the Ed25519 key of the node, and a login rate limiter.
package auth

import (
	"context"
	"fmt"
	"time"

	"github.com/SpliceFS/common/api"
)

// dummyHash is verified when the user is unknown or disabled, so that the response time
// does not reveal which names exist. It uses the same parameters as HashPassword.
const dummyHash = "$argon2id$v=19$m=65536,t=3,p=4$MDEyMzQ1Njc4OWFiY2RlZg$0a8C2hCNSJXYtFEEWDSZWM1bCCoWQ8Fw3yE1V3jklIY"

// Authenticator logs users in and checks their tokens. It is safe for concurrent use.
type Authenticator struct {
	users   *Users
	tokens  *Tokens
	limiter *Limiter
	verify  func(encoded, password string) (bool, error)
	// slots is a counting semaphore: each password verification holds one slot.
	slots chan struct{}
}

// New returns an Authenticator built from its parts. Each password verification uses
// 64 MiB, so slots bounds how many logins may be verified at once; a value below 1 is
// treated as 1.
func New(users *Users, tokens *Tokens, limiter *Limiter, slots int) *Authenticator {
	return &Authenticator{
		users:   users,
		tokens:  tokens,
		limiter: limiter,
		verify:  VerifyPassword,
		slots:   make(chan struct{}, max(slots, 1)),
	}
}

// Login checks the password of user and issues a token. It returns api.ErrRateLimited when
// clientAddr has used up its attempts, and api.ErrBadCredentials for an unknown user, a
// disabled user or a wrong password. Any other failure is returned wrapped and is not
// api.ErrBadCredentials.
//
// After the rate limit check, Login waits for a free verification slot. If ctx ends first
// it returns ctx.Err() wrapped, so a client that gave up does not queue for memory.
func (a *Authenticator) Login(
	ctx context.Context, clientAddr, user, password string,
) (string, time.Time, error) {
	if !a.limiter.Allow(clientAddr) {
		return "", time.Time{}, api.ErrRateLimited
	}
	if !a.acquire(ctx) {
		return "", time.Time{}, fmt.Errorf("login: %w", ctx.Err())
	}
	defer a.release()
	account, found := a.users.Lookup(user)
	if !found || account.Disabled {
		return "", time.Time{}, a.reject(password)
	}
	ok, err := a.verify(account.PasswordHash, password)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("login: user %q: %w", account.Name, err)
	}
	if !ok {
		return "", time.Time{}, api.ErrBadCredentials
	}
	token, expires, err := a.tokens.Issue(account.ID)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("login: user %q: %w", account.Name, err)
	}
	return token, expires, nil
}

// Verify checks a token and returns its claims. The token only names the user: the name and
// the shares are those of the state of the user, so that a changed share list or a disabled
// user takes effect for tokens already issued. It returns the errors of Tokens.Verify, and
// api.ErrInvalidToken for a token whose user is gone or disabled. A user file that cannot be
// checked is returned wrapped and is neither token error, so the request fails and is not
// let through.
func (a *Authenticator) Verify(token string) (api.Claims, error) {
	claims, err := a.tokens.Verify(token)
	if err != nil {
		return api.Claims{}, err
	}
	user, found, err := a.users.Current(claims.UserID)
	if err != nil {
		return api.Claims{}, fmt.Errorf("check user of token: %w", err)
	}
	if !found || user.Disabled {
		return api.Claims{}, invalidToken("the user is gone or disabled")
	}
	return api.Claims{User: user.Name, Shares: user.Shares, Expires: claims.Expires}, nil
}

// acquire takes a verification slot. It returns false if ctx ends first.
func (a *Authenticator) acquire(ctx context.Context) bool {
	select {
	case a.slots <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

// release returns a slot taken by acquire.
func (a *Authenticator) release() {
	<-a.slots
}

// reject spends the time of one password verification against dummyHash and returns
// api.ErrBadCredentials.
func (a *Authenticator) reject(password string) error {
	//nolint:errcheck // only the time of the call matters, and the reply must not depend on its result
	_, _ = a.verify(dummyHash, password)
	return api.ErrBadCredentials
}
