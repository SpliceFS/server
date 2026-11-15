package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/SpliceFS/server/auth"
)

// bearerPrefix starts the value of an Authorization header that carries a token.
const bearerPrefix = "Bearer "

// authenticated runs next only for a request with a valid token. Everything it answers
// is private to the user, so shared caches must not keep it.
func (s *server) authenticated(next func(http.ResponseWriter, *http.Request, auth.Claims)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := s.claims(w, r)
		if !ok {
			return
		}
		setAccessUser(r.Context(), claims.User)
		w.Header().Set("Cache-Control", "private, no-cache")
		next(w, r, claims)
	}
}

// claims returns the claims of the request's token. If there are none, it has already
// answered the request.
func (s *server) claims(w http.ResponseWriter, r *http.Request) (auth.Claims, bool) {
	token, ok := bearerToken(r.Header.Get("Authorization"))
	if !ok {
		unauthorized(w, "invalid token")
		return auth.Claims{}, false
	}
	claims, err := s.auth.Verify(token)
	switch {
	case err == nil:
		return claims, true
	case errors.Is(err, auth.ErrExpiredToken):
		unauthorized(w, "token expired")
	case errors.Is(err, auth.ErrInvalidToken):
		unauthorized(w, "invalid token")
	default:
		// For example an unreadable keys file: the operator's problem, not the client's.
		s.internalError(w, r, err)
	}
	return auth.Claims{}, false
}

// bearerToken extracts the token from an Authorization header value.
func bearerToken(header string) (string, bool) {
	if len(header) <= len(bearerPrefix) || !strings.EqualFold(header[:len(bearerPrefix)], bearerPrefix) {
		return "", false
	}
	token := header[len(bearerPrefix):]
	return token, !strings.ContainsAny(token, " \t")
}

// unauthorized answers 401 and names the authentication scheme.
func unauthorized(w http.ResponseWriter, message string) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	writeError(w, http.StatusUnauthorized, message)
}

// forShare runs next for the share named in the request path, if the token allows it
// and the share exists. The permission is checked first, so that a token cannot be used
// to find out which other shares exist.
func (s *server) forShare(next func(http.ResponseWriter, *http.Request, Share)) http.HandlerFunc {
	return s.authenticated(func(w http.ResponseWriter, r *http.Request, claims auth.Claims) {
		name := r.PathValue("share")
		if !claims.Allows(name) {
			writeError(w, http.StatusForbidden, "share not allowed")
			return
		}
		shared, ok := s.shares[name]
		if !ok {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		next(w, r, shared)
	})
}
