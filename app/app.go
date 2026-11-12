// Package app is the composition root of the SpliceFS server: it builds every part from a
// configuration, wires them together, serves HTTP on the configured listeners and shuts
// down cleanly. It is the only place that knows the concrete types of the other server
// packages.
package app

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/SpliceFS/common/wire"
	"github.com/SpliceFS/server/api"
	"github.com/SpliceFS/server/auth"
	"github.com/SpliceFS/server/cluster"
	"github.com/SpliceFS/server/config"
)

// loginSlots is how many password verifications may run at once. Each one needs 64 MiB.
const loginSlots = 4

// shutdownGrace is how long a shutdown waits for requests in progress.
const shutdownGrace = 10 * time.Second

// Options are the parts of a run that do not come from the configuration.
type Options struct {
	// Version is the program version, reported by the health endpoint.
	Version string
	// Logger receives the server's log. The default discards everything.
	Logger *slog.Logger
	// Ready, if set, is called once with the bound address of every listener, in the
	// order of the configuration, when the server is able to accept connections.
	Ready func([]net.Addr)
}

// Run serves the API described by cfg, keeping its data in stateDir, until ctx is
// cancelled, then shuts down and returns nil. It fails at once, before it accepts any
// connection, if a share cannot be opened, the state directory cannot be written, this
// node's signing key cannot be made or read, a certificate cannot be loaded or created or a
// listener cannot be opened. It returns an error if serving fails. When it returns, everything it started has stopped.
func Run(ctx context.Context, cfg *config.Config, stateDir string, opts Options) (err error) {
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	shares, err := openShares(cfg)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, shares.Close()) }()

	handler, err := newHandler(cfg, layout(stateDir), opts.Version, logger, shares)
	if err != nil {
		return err
	}
	listeners, err := listen(ctx, cfg.Listen, sync.OnceValues(func() (tls.Certificate, error) {
		return localCertificate(stateDir)
	}))
	if err != nil {
		return err
	}
	srv := newServer(handler, logger)
	return serve(ctx, srv, listeners, shutdownGrace, opts.Ready)
}

// newHandler builds the API handler. It first makes what this node needs in the state
// directory, so that a server with nothing configured and nothing stored starts, and it
// refuses to start if it cannot make or read its own key or announce itself.
func newHandler(
	cfg *config.Config, data dataPaths, version string, logger *slog.Logger, shares *shareSet,
) (http.Handler, error) {
	if err := data.makeFolders(); err != nil {
		return nil, err
	}
	nodeID, err := data.nodeID(rand.Reader)
	if err != nil {
		return nil, err
	}
	tokens := auth.NewTokens(data.keysDir, nodeID, cfg.Auth.TokenTTL.Std(), time.Now, logger)
	if err := tokens.EnsureOwnKey(); err != nil {
		return nil, err
	}
	self := wire.Node{Name: cfg.Cluster.Node, URL: cfg.Cluster.URL}
	if err := cluster.Announce(data.nodesDir, nodeID, self); err != nil {
		return nil, err
	}
	nodes := namedCluster{name: cfg.Cluster.Name, nodes: cluster.NewLoader(data.nodesDir, logger)}
	limiter := auth.NewLimiter(cfg.Auth.LoginRate.Count, cfg.Auth.LoginRate.Per, time.Now)
	return api.NewHandler(api.Options{
		BasePath:       cfg.BasePath,
		Node:           cfg.Cluster.Node,
		Version:        version,
		Shares:         shares.byName,
		Auth:           auth.New(auth.NewUsers(data.usersDir), tokens, limiter, loginSlots),
		Nodes:          nodes,
		TrustedProxies: cfg.TrustedProxies,
		Logger:         logger,
	}), nil
}
