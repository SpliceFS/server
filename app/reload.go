package app

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/SpliceFS/server/config"
)

// runner is the serving state of a run, the part that a reload changes. Only the goroutine
// that runs serve touches it.
type runner struct {
	srv       *http.Server
	handlers  *live
	build     builder
	endpoints map[string]*endpoint // the listeners being served, by configured address
	// failures receives the first listener failure. Later ones are dropped: the first one
	// already stops the server.
	failures chan error
	wg       sync.WaitGroup
	logger   *slog.Logger
	opts     Options
}

// listenPlan is what a configuration changes about the listeners.
type listenPlan struct {
	kept    map[*endpoint]*tls.Config // already open, with the TLS settings they get
	added   []*endpoint               // opened for the plan, not served yet
	removed []*endpoint               // open, and no longer configured
}

// serve runs the server on listeners until ctx is cancelled or a listener fails, calling
// Ready first, and applies the configuration again on every reload signal. After
// cancellation it waits up to grace for requests in progress and then closes their
// connections; that is a clean shutdown and returns nil. After a failure it closes
// everything at once and returns the failure. It always waits for its goroutines.
func (r *runner) serve(ctx context.Context, listeners []*endpoint, grace time.Duration) error {
	if r.opts.Ready != nil {
		r.opts.Ready(addresses(listeners))
	}
	for _, e := range listeners {
		r.start(e)
	}
	defer r.wg.Wait()

	for {
		select {
		case <-ctx.Done():
			return shutdown(ctx, r.srv, grace)
		case err := <-r.failures:
			return errors.Join(err, r.srv.Close())
		case <-r.opts.Reload:
			r.reload(ctx)
		}
	}
}

// start serves e in the background.
func (r *runner) start(e *endpoint) {
	r.endpoints[e.address] = e
	r.wg.Go(func() {
		err := r.srv.Serve(e)
		// A retired endpoint was closed on purpose, which is not a failure.
		if errors.Is(err, http.ErrServerClosed) || e.isRetired() {
			return
		}
		select {
		case r.failures <- fmt.Errorf("serve on %s: %w", e.Addr(), err):
		default:
		}
	})
}

// reload applies the configuration as it is now. Everything that can fail happens before
// anything changes, so a configuration that cannot be applied leaves the server exactly as
// it was. Once it is applied, new requests use it and requests in progress finish on the
// configuration they started with.
func (r *runner) reload(ctx context.Context) {
	cfg, err := r.opts.Load()
	if err != nil {
		r.logger.Error("configuration not applied", "error", err)
		return
	}
	listeners, err := r.plan(ctx, cfg.Listen)
	if err != nil {
		r.logger.Error("configuration not applied", "error", err)
		return
	}
	// The generation comes last because building it announces this node to the cluster,
	// which is the one step here that cannot be taken back.
	next, err := r.build.generation(cfg)
	if err != nil {
		err = errors.Join(err, closeAll(listeners.added))
		r.logger.Error("configuration not applied", "error", err)
		return
	}

	r.handlers.replace(next)
	for e, tlsConfig := range listeners.kept {
		e.setTLS(tlsConfig)
	}
	for _, e := range listeners.added {
		r.start(e)
	}
	for _, e := range listeners.removed {
		delete(r.endpoints, e.address)
		// Connections it accepted stay open and finish their requests. A close error of a
		// listener that is going away changes nothing.
		_ = e.retire()
	}
	if r.opts.Applied != nil {
		r.opts.Applied(cfg)
	}
	r.logger.Info("configuration applied", "listeners", len(r.endpoints))
}

// plan compares entries with the listeners being served. It loads every certificate and
// opens every added address, so that a plan that is returned can no longer fail; on an
// error it closes what it opened.
func (r *runner) plan(ctx context.Context, entries []config.Endpoint) (listenPlan, error) {
	p := listenPlan{kept: make(map[*endpoint]*tls.Config)}
	wanted := make(map[string]bool, len(entries))
	for _, entry := range entries {
		address := listenAddress(entry)
		wanted[address] = true
		e, err := r.planOne(ctx, entry, address, p.kept)
		if err != nil {
			return listenPlan{}, errors.Join(
				fmt.Errorf("listen on %s: %w", address, err), closeAll(p.added))
		}
		if e != nil {
			p.added = append(p.added, e)
		}
	}
	for address, e := range r.endpoints {
		if !wanted[address] {
			p.removed = append(p.removed, e)
		}
	}
	return p, nil
}

// planOne handles one entry: an address that is already open is kept and gets the TLS
// settings of the entry, any other is opened and returned. The certificate is loaded
// before the port is bound, so a bad certificate does not leave a port open.
func (r *runner) planOne(
	ctx context.Context, entry config.Endpoint, address string, kept map[*endpoint]*tls.Config,
) (*endpoint, error) {
	tlsConfig, err := tlsConfigFor(entry, r.build.ownCert)
	if err != nil {
		return nil, err
	}
	if e, ok := r.endpoints[address]; ok {
		kept[e] = tlsConfig
		return nil, nil
	}
	return openEndpoint(ctx, address, tlsConfig)
}
