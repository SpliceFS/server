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
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/SpliceFS/common/api"
	"github.com/SpliceFS/common/wire"
	"github.com/SpliceFS/server/auth"
	"github.com/SpliceFS/server/cluster"
	"github.com/SpliceFS/server/config"
	"github.com/SpliceFS/server/internal/digestcache"
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
	// Reload, if set, delivers a signal each time the configuration is to be applied again
	// without a restart. Load must be set with it.
	Reload <-chan os.Signal
	// Load reads the configuration for a reload.
	Load func() (*config.Config, error)
	// Applied, if set, is called with the new configuration after a reload took effect.
	Applied func(*config.Config)
}

// Run serves the API described by cfg, keeping its data in stateDir, until ctx is
// cancelled, then shuts down and returns nil. It first makes what this node needs in the
// state directory, so that a server with nothing configured and nothing stored starts. It
// fails at once, before it accepts any connection, if a share cannot be opened, the state
// directory or the digest folder in it cannot be written, this
// node's signing key cannot be made or read, a certificate cannot be loaded or created or a
// listener cannot be opened. It returns an error if serving fails. When it returns,
// everything it started has stopped.
//
// A signal on opts.Reload applies the configuration that opts.Load returns to the running
// server; see runner.reload.
func Run(ctx context.Context, cfg *config.Config, stateDir string, opts Options) (err error) {
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	data := layout(stateDir)
	err = data.makeFolders()
	if err != nil {
		return err
	}
	// One store for every share and every reload: it empties its temporary folder when it
	// opens, which is safe only before any share writes to it.
	digests, err := digestcache.Open(data.digestsDir, logger)
	if err != nil {
		return err //nolint:wrapcheck // the error says it was opening the digest folder
	}
	defer func() { err = errors.Join(err, wrap(digests.Close(), "close the digest folder")) }()
	r := &runner{
		build: builder{
			data:    data,
			digests: digests,
			version: opts.Version,
			logger:  logger,
			ownCert: sync.OnceValues(func() (tls.Certificate, error) {
				return localCertificate(stateDir)
			}),
			clusterKey: cluster.NewKeyReader(data.clusterDir, logger),
		},
		endpoints: make(map[string]*endpoint),
		failures:  make(chan error, 1),
		logger:    logger,
		opts:      opts,
	}
	first, err := r.build.generation(cfg)
	if err != nil {
		return err
	}
	r.handlers = newLive(first, logger)
	defer func() { err = errors.Join(err, r.handlers.close()) }()

	changes, err := r.plan(ctx, cfg.Listen)
	if err != nil {
		return err
	}
	r.srv = newServer(r.handlers, logger)
	r.sweep(ctx, first)
	return r.serve(ctx, changes.added, shutdownGrace)
}

// builder makes a generation from a configuration. Everything in it stays the same for the
// whole run.
type builder struct {
	data    dataPaths
	digests *digestcache.Store
	version string
	logger  *slog.Logger
	// ownCert is the certificate the server made for itself, created on first use.
	ownCert certificateSource
	// clusterKey reads the cluster key of the shared state as the file changes, one reader for
	// the whole run so that a key that stops being readable is still the last good one after a
	// reload.
	clusterKey *cluster.KeyReader
}

// heal makes the state folders that were removed while the server ran, as the start made
// them, and points the digest store at its folder again.
func (b *builder) heal() error {
	if err := b.data.makeFolders(); err != nil {
		return err
	}
	return b.digests.Reopen() //nolint:wrapcheck // the error says it was the digest folder
}

// generation opens the shares of cfg and builds the handler that serves them.
func (b *builder) generation(cfg *config.Config) (*generation, error) {
	shares, err := openShares(cfg, b.digests)
	if err != nil {
		return nil, err
	}
	handler, err := newHandler(cfg, &b.data, b.version, b.logger, shares)
	if err != nil {
		return nil, errors.Join(err, shares.Close())
	}
	return &generation{handler: handler, closer: shares, files: shares.files}, nil
}

// newHandler builds the API handler. It refuses to start if it cannot make or read its own
// keys or announce itself.
func newHandler(
	cfg *config.Config, data *dataPaths, version string, logger *slog.Logger, shares *shareSet,
) (http.Handler, error) {
	nodeID, err := data.nodeID(time.Now, rand.Reader)
	if err != nil {
		return nil, err
	}
	// A start and every reload pass here, so the old keys file of this node is gone after both.
	err = data.removeOldKeys(nodeID)
	if err != nil {
		return nil, err
	}
	// Made or read together with the node id, so that a bad file stops the start.
	keys, err := data.nodeKeys(rand.Reader)
	if err != nil {
		return nil, err
	}
	public := keys.public()
	self := wire.Node{
		Name: cfg.Cluster.Node, URL: cfg.Cluster.URL, SignKey: public.sign, EncryptKey: public.encrypt,
	}
	if err := cluster.Announce(data.nodesDir, nodeID, self); err != nil {
		return nil, fmt.Errorf("announce node: %w", err)
	}
	loader := cluster.NewLoader(data.nodesDir, logger)
	tokens := auth.NewTokens(nodeID, keys.sign, loader, cfg.Auth.TokenTTL.Std(), time.Now)
	limiter := auth.NewLimiter(cfg.Auth.LoginRate.Count, cfg.Auth.LoginRate.Per, time.Now)
	return api.NewHandler(api.Options{
		BasePath:       cfg.BasePath,
		Node:           cfg.Cluster.Node,
		Version:        version,
		Shares:         shares.byName,
		Auth:           auth.New(auth.NewUsers(data.usersDir, time.Now, logger), tokens, limiter, loginSlots),
		Nodes:          namedCluster{name: cfg.Cluster.Name, nodes: loader},
		TrustedProxies: cfg.TrustedProxies,
		Logger:         logger,
	}), nil
}

// wrap adds what the code was doing to err. It returns nil for a nil err, so that a call can
// be passed straight in.
func wrap(err error, doing string) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", doing, err)
}
