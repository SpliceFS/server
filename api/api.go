// Package api implements the HTTP API of the SpliceFS server, version 1: health, login,
// the node list, directory listings, file contents and free space. It depends on small
// interfaces for shares, logins and the node list, so it knows nothing about how they
// are stored.
package api

import (
	"context"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/netip"
	"slices"
	"time"

	"github.com/BlueRival/splicefs/internal/wire"
	"github.com/BlueRival/splicefs/server/auth"
	"github.com/BlueRival/splicefs/server/share"
)

// Share is read-only access to one shared directory. Errors that mean "no such name"
// are share.ErrNotFound, share.ErrNotDir and share.ErrIsDir; any other error is
// reported to the client as an internal error.
type Share interface {
	// Stat describes the file or directory at name; "" is the share root.
	Stat(name string) (share.Info, error)
	// List returns the visible entries of a directory, sorted by name.
	List(name string) ([]share.Info, error)
	// Open opens a regular file. The File's Info describes the file that was opened.
	Open(name string) (File, error)
	// HeadDigest returns the head digest and the Info of a regular file.
	HeadDigest(name string) (string, share.Info, error)
	// Space reports the size and free space of the file system holding the share.
	Space() (wire.Space, error)
}

// File is an open file. The caller closes it.
type File interface {
	io.ReadSeeker
	io.Closer
	// Info describes the file that was actually opened.
	Info() share.Info
}

// Authenticator logs users in and checks their tokens; *auth.Authenticator is one.
type Authenticator interface {
	// Login returns a token and its expiry, or auth.ErrBadCredentials or
	// auth.ErrRateLimited.
	Login(ctx context.Context, clientAddr, user, password string) (string, time.Time, error)
	// Verify returns the claims of a token, or auth.ErrInvalidToken or
	// auth.ErrExpiredToken.
	Verify(token string) (auth.Claims, error)
}

// NodeLister returns the node list of the cluster.
type NodeLister interface {
	Nodes() (wire.Nodes, error)
}

// Options configures the handler returned by NewHandler.
type Options struct {
	// BasePath is the URL prefix of the API: "" or a path that starts with a slash,
	// does not end with one and contains no pattern syntax.
	BasePath string
	// Node is this node's name, and Version the program version. Both are reported by
	// the health endpoint.
	Node, Version string
	// Shares maps a share name to the share.
	Shares map[string]Share
	// Auth logs users in and verifies tokens.
	Auth Authenticator
	// Nodes supplies the node list.
	Nodes NodeLister
	// TrustedProxies are the networks whose X-Forwarded-For header is believed.
	TrustedProxies []netip.Prefix
	// Logger receives the access log and errors. The default discards everything.
	Logger *slog.Logger
	// Now returns the current time, for access log durations. The default is time.Now.
	Now func() time.Time
}

// server holds what the endpoint handlers share.
type server struct {
	node       string
	version    string
	base       string
	shares     map[string]Share
	shareNames []string
	auth       Authenticator
	nodes      NodeLister
	log        *slog.Logger
}

// NewHandler returns the handler of the API. It is safe for concurrent use.
func NewHandler(opts Options) http.Handler {
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	s := &server{
		node:       opts.Node,
		version:    opts.Version,
		base:       opts.BasePath,
		shares:     opts.Shares,
		shareNames: slices.Sorted(maps.Keys(opts.Shares)),
		auth:       opts.Auth,
		nodes:      opts.Nodes,
		log:        log,
	}
	handler := noSniff(s.routes())
	handler = recoverPanics(log, handler)
	handler = logAccess(log, now, handler)
	return resolveClient(opts.TrustedProxies, handler)
}
