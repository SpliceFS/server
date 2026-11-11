package api

import (
	"net/http"
	"strings"
)

// Allow header values. The mux sends HEAD requests to the handler of the GET pattern.
const (
	allowGet  = "GET, HEAD"
	allowPost = "POST"
)

// routes returns the router for every endpoint under the base path.
func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	s.handle(mux, http.MethodGet, "/v1/health", s.health)
	s.handle(mux, http.MethodPost, "/v1/login", s.login)
	s.handle(mux, http.MethodGet, "/v1/nodes", s.authenticated(s.listNodes))
	s.handle(mux, http.MethodGet, "/v1/tree/{$}", s.authenticated(s.listShares))
	s.handle(mux, http.MethodGet, "/v1/tree/{share}", s.forShare(s.listDir))
	s.handle(mux, http.MethodGet, "/v1/tree/{share}/{dir...}", s.forShare(s.listDir))
	s.handle(mux, http.MethodGet, "/v1/file/{share}/{name...}", s.forShare(s.file))
	s.handle(mux, http.MethodGet, "/v1/space/{share}", s.forShare(s.space))

	// Without these, the mux answers a request for a subtree root that lacks its
	// trailing slash with a redirect instead of a JSON error.
	mux.HandleFunc(s.base+"/v1/tree", notFound)
	mux.HandleFunc(s.base+"/v1/file/{share}", notFound)
	mux.HandleFunc("/", notFound)
	return canonicalPaths(mux)
}

// handle registers handler for method and pattern below the base path. Other methods on
// the same pattern get 405 and an Allow header.
func (s *server) handle(mux *http.ServeMux, method, pattern string, handler http.HandlerFunc) {
	full := s.base + pattern
	mux.HandleFunc(method+" "+full, handler)
	allow := allowGet
	if method == http.MethodPost {
		allow = allowPost
	}
	mux.HandleFunc(full, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Allow", allow)
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	})
}

// notFound answers 404 for every path that is not an endpoint.
func notFound(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusNotFound, "not found")
}

// canonicalPaths answers 404 for a path that has dot segments or empty segments. The mux
// would redirect such a request to the cleaned path, and an API has no use for redirects.
func canonicalPaths(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isCanonical(r.URL.Path) {
			notFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isCanonical reports whether p is an absolute path that the mux would not rewrite.
func isCanonical(p string) bool {
	if !strings.HasPrefix(p, "/") {
		return false
	}
	return !strings.Contains(p, "//") && !hasDotSegment(p)
}

// hasDotSegment reports whether p has a "." or ".." segment.
func hasDotSegment(p string) bool {
	for segment := range strings.SplitSeq(p, "/") {
		if segment == "." || segment == ".." {
			return true
		}
	}
	return false
}
