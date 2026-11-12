package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/SpliceFS/common/wire"
	"github.com/SpliceFS/server/auth"
	"github.com/SpliceFS/server/share"
)

// maxLoginBody is the largest login request accepted. A user name and a password are
// far smaller; the limit keeps a client from making the server buffer a large body.
const maxLoginBody = 4 << 10

// loginRetryAfter is how long a rate limited client is told to wait, in seconds.
const loginRetryAfter = "60"

// health answers GET /v1/health.
func (s *server) health(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, r, wire.Health{Status: "ok", Version: s.version, Node: s.node})
}

// login answers POST /v1/login.
func (s *server) login(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	req, err := readLogin(w, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "malformed login request")
		return
	}
	token, expires, err := s.auth.Login(r.Context(), clientAddr(r.Context()), req.User, req.Password)
	switch {
	case err == nil:
		s.writeJSON(w, r, wire.LoginResponse{Token: token, Expires: expires})
	case errors.Is(err, auth.ErrBadCredentials):
		writeError(w, http.StatusUnauthorized, "bad credentials")
	case errors.Is(err, auth.ErrRateLimited):
		w.Header().Set("Retry-After", loginRetryAfter)
		writeError(w, http.StatusTooManyRequests, "too many login attempts")
	default:
		s.internalError(w, r, err)
	}
}

// readLogin reads and checks the body of a login request.
func readLogin(w http.ResponseWriter, r *http.Request) (wire.LoginRequest, error) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxLoginBody))
	if err != nil {
		return wire.LoginRequest{}, err
	}
	return parseLogin(data)
}

// parseLogin decodes data, which must be one JSON object that has only known fields.
func parseLogin(data []byte) (wire.LoginRequest, error) {
	var req wire.LoginRequest
	data = bytes.TrimSpace(data)
	// Decoding null into a struct succeeds and changes nothing, which would turn
	// a body of "null" into a login attempt by the empty user.
	if !bytes.HasPrefix(data, []byte("{")) {
		return req, errors.New("not a JSON object")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return req, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return req, errors.New("data after the JSON object")
	}
	return req, nil
}

// listNodes answers GET /v1/nodes.
func (s *server) listNodes(w http.ResponseWriter, r *http.Request, _ auth.Claims) {
	nodes, err := s.nodes.Nodes()
	if err != nil {
		s.internalError(w, r, fmt.Errorf("read node list: %w", err))
		return
	}
	s.writeJSON(w, r, nodes)
}

// listShares answers GET /v1/tree/: the shares the token allows, as directories.
func (s *server) listShares(w http.ResponseWriter, r *http.Request, claims auth.Claims) {
	entries := []wire.Entry{}
	for _, name := range s.shareNames {
		if !claims.Allows(name) {
			continue
		}
		info, err := s.shares[name].Stat("")
		if err != nil {
			s.internalError(w, r, fmt.Errorf("stat share %q: %w", name, err))
			return
		}
		entries = append(entries, wire.Entry{Name: name, Dir: true, MTime: info.MTime})
	}
	s.writeJSON(w, r, wire.Listing{Entries: entries})
}

// listDir answers GET /v1/tree/{share}/{dir...}.
func (s *server) listDir(w http.ResponseWriter, r *http.Request, sh Share) {
	infos, err := sh.List(strings.TrimSuffix(r.PathValue("dir"), "/"))
	if err != nil {
		s.failLookup(w, r, err, share.ErrNotFound, share.ErrNotDir)
		return
	}
	entries := make([]wire.Entry, 0, len(infos))
	for _, info := range infos {
		entries = append(entries, wire.Entry{
			Name: info.Name, Dir: info.Dir, Size: info.Size, MTime: info.MTime, ETag: info.ETag,
		})
	}
	s.writeJSON(w, r, wire.Listing{Entries: entries})
}

// space answers GET /v1/space/{share}.
func (s *server) space(w http.ResponseWriter, r *http.Request, sh Share) {
	space, err := sh.Space()
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.writeJSON(w, r, space)
}

// failLookup answers 404 when err is one of missing, and 500 for any other error.
func (s *server) failLookup(w http.ResponseWriter, r *http.Request, err error, missing ...error) {
	for _, target := range missing {
		if errors.Is(err, target) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
	}
	s.internalError(w, r, err)
}
