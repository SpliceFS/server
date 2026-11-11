// Package auth implements logins for the SpliceFS server: argon2id password hashes, user
// files, signed tokens with rotating keys, and a login rate limiter.
package auth

import (
	"errors"
	"fmt"
	"time"
)

var (
	// ErrBadCredentials means the user is unknown or disabled, or the password is wrong.
	// The three cases are deliberately indistinguishable.
	ErrBadCredentials = errors.New("bad credentials")
	// ErrRateLimited means the client address has made too many login attempts.
	ErrRateLimited = errors.New("too many login attempts")
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
}

// New returns an Authenticator built from its parts.
func New(users *Users, tokens *Tokens, limiter *Limiter) *Authenticator {
	return &Authenticator{users: users, tokens: tokens, limiter: limiter, verify: VerifyPassword}
}

// Login checks the password of user and issues a token. It returns ErrRateLimited when
// clientAddr has used up its attempts, and ErrBadCredentials for an unknown user, a
// disabled user or a wrong password. Any other failure, such as an unreadable user file or
// keys file, is returned wrapped and is not ErrBadCredentials.
func (a *Authenticator) Login(clientAddr, user, password string) (token string, expires time.Time, err error) {
	if !a.limiter.Allow(clientAddr) {
		return "", time.Time{}, ErrRateLimited
	}
	u, err := a.users.Lookup(user)
	if errors.Is(err, ErrNoUser) {
		return "", time.Time{}, a.reject(password)
	}
	if err != nil {
		return "", time.Time{}, fmt.Errorf("login: %w", err)
	}
	if u.Disabled {
		return "", time.Time{}, a.reject(password)
	}
	ok, err := a.verify(u.PasswordHash, password)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("login: user %q: %w", u.Name, err)
	}
	if !ok {
		return "", time.Time{}, ErrBadCredentials
	}
	token, expires, err = a.tokens.Issue(u.Name, u.Shares)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("login: user %q: %w", u.Name, err)
	}
	return token, expires, nil
}

// reject spends the time of one password verification against dummyHash and returns
// ErrBadCredentials.
func (a *Authenticator) reject(password string) error {
	_, _ = a.verify(dummyHash, password)
	return ErrBadCredentials
}

// Verify checks a token and returns its claims; see Tokens.Verify.
func (a *Authenticator) Verify(token string) (Claims, error) {
	return a.tokens.Verify(token)
}
