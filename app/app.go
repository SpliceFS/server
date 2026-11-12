// Package app is the composition root of the SpliceFS server: it builds every part from a
// configuration, wires them together, serves HTTP on the configured listeners and shuts
// down cleanly. It is the only place that knows the concrete types of the other server
// packages.
package app

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

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
// connection, if a share cannot be opened, the keys file cannot be read, the node list is
// invalid, a certificate cannot be loaded or created or a listener cannot be opened. It
// returns an error if serving fails. When it returns, everything it started has stopped.
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

// newHandler builds the API handler. It reads the keys file and the node list once, so
// that a mistake in either stops the server at start instead of failing every request.
func newHandler(
	cfg *config.Config, data dataPaths, version string, logger *slog.Logger, shares *shareSet,
) (http.Handler, error) {
	tokens := auth.NewTokens(data.keysFile, cfg.Auth.TokenTTL.Std(), time.Now)
	if _, _, err := tokens.Issue("startup-check", nil); err != nil {
		return nil, fmt.Errorf("check keys file: %w", err)
	}
	nodes := namedCluster{name: cfg.Cluster.Name, nodes: cluster.NewLoader(data.nodesFile)}
	if _, err := nodes.Nodes(); err != nil {
		return nil, fmt.Errorf("check node list: %w", err)
	}
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
