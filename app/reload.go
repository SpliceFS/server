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
	// stopSweep ends the sweep of the digest entries that runs, and returns when it has ended.
	stopSweep func()
	wg        sync.WaitGroup
	logger    *slog.Logger
	opts      Options
	// renewals receives once a day, when the node certificate is checked. Nil when there is
	// nothing to check.
	renewals <-chan time.Time
}

// listenPlan is what a configuration changes about the listeners.
type listenPlan struct {
	kept    map[*endpoint]*tls.Config // already open, with the TLS settings they get
	added   []*endpoint               // opened for the plan, not served yet
	removed []*endpoint               // open, and no longer configured
}

// serve runs the server on listeners until ctx is cancelled or a listener fails, and
// applies the configuration again on every reload signal. After cancellation it waits up to
// grace for requests in progress and then closes their connections; that is a clean
// shutdown and returns nil. After a failure it closes everything at once and returns the
// failure. It always waits for its goroutines.
func (r *runner) serve(ctx context.Context, listeners []*endpoint, grace time.Duration) error {
	for _, listener := range listeners {
		r.start(listener)
	}
	defer r.wg.Wait()
	defer r.stopSweep()

	for {
		select {
		case <-ctx.Done():
			return shutdown(ctx, r.srv, grace)
		case err := <-r.failures:
			return errors.Join(err, r.srv.Close())
		case <-r.opts.Reload:
			r.reload(ctx)
		case <-r.renewals:
			r.renewCertificate(ctx)
		}
	}
}

// renewCertificate is the daily check of the node certificate. A check that fails leaves the
// certificate that is served, which is valid for some time yet, and the next check tries again.
func (r *runner) renewCertificate(ctx context.Context) {
	if err := r.build.ownCert.renew(); err != nil {
		r.logger.WarnContext(ctx, "cannot renew the node certificate", slog.Any("error", err))
	}
}

// start serves listener in the background. Every listener begins serving here, at startup
// and on a reload, so this is the one place that tells the operator where it is reachable.
// Binding is not serving: a reload binds while it plans and may throw the plan away.
func (r *runner) start(listener *endpoint) {
	r.endpoints[listener.address] = listener
	r.logger.Info("listening", slog.String("address", listener.Addr().String()))
	r.wg.Go(func() {
		err := r.srv.Serve(listener)
		// A retired endpoint was closed on purpose, which is not a failure.
		if errors.Is(err, http.ErrServerClosed) || listener.isRetired() {
			return
		}
		select {
		case r.failures <- fmt.Errorf("serve on %s: %w", listener.Addr(), err):
		default:
		}
	})
}

// sweep removes, in the background, the digest entries that no request of gen can use. It
// runs after the start and after each reload, as a reload can remove shares and change what
// they hide. Only one runs at a time, and serve ends it before it returns.
func (r *runner) sweep(ctx context.Context, gen *generation) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		err := r.build.digests.Sweep(ctx, gen.files)
		if err == nil {
			r.logger.InfoContext(ctx, "digest entries swept")
		} else if !errors.Is(err, context.Canceled) {
			r.logger.WarnContext(ctx, "cannot sweep digest entries", slog.Any("error", err))
		}
	}()
	r.stopSweep = func() {
		cancel()
		<-done
	}
}

// reload applies the configuration as it is now. Everything that can fail happens before
// anything changes, so a configuration that cannot be applied leaves the server exactly as
// it was. Once it is applied, new requests use it and requests in progress finish on the
// configuration they started with.
func (r *runner) reload(ctx context.Context) {
	cfg, err := r.opts.Load()
	if err != nil {
		r.logger.ErrorContext(ctx, "configuration not applied", slog.Any("error", err))
		return
	}
	certificate, err := r.build.prepare(cfg)
	if err != nil {
		r.logger.ErrorContext(ctx, "configuration not applied", slog.Any("error", err))
		return
	}
	changes, err := r.plan(ctx, cfg.Listen)
	if err != nil {
		r.logger.ErrorContext(ctx, "configuration not applied", slog.Any("error", err))
		return
	}
	// The generation comes last because building it announces this node to the cluster,
	// which is the one step here that cannot be taken back.
	next, err := r.build.generation(cfg)
	if err != nil {
		err = errors.Join(err, closeAll(changes.added))
		r.logger.ErrorContext(ctx, "configuration not applied", slog.Any("error", err))
		return
	}

	// The sweep of the old configuration uses its shares, which close once it is replaced.
	r.stopSweep()
	r.build.ownCert.commit(certificate)
	r.handlers.replace(next)
	r.sweep(ctx, next)
	for listener, tlsConfig := range changes.kept {
		listener.setTLS(tlsConfig)
	}
	for _, listener := range changes.added {
		r.start(listener)
	}
	for _, listener := range changes.removed {
		delete(r.endpoints, listener.address)
		// Connections it accepted stay open and finish their requests. A close error of a
		// listener that is going away changes nothing, so it is logged and not acted on.
		if err := listener.retire(); err != nil {
			r.logger.WarnContext(ctx, "close removed listener", slog.Any("error", err))
		}
	}
	if r.opts.Applied != nil {
		r.opts.Applied(cfg)
	}
	r.logger.InfoContext(ctx, "configuration applied", slog.Int("listeners", len(r.endpoints)))
}

// plan compares entries with the listeners being served. It loads every certificate and
// opens every added address, so that a plan that is returned can no longer fail; on an
// error it closes what it opened.
func (r *runner) plan(ctx context.Context, entries []config.Endpoint) (listenPlan, error) {
	changes := listenPlan{kept: make(map[*endpoint]*tls.Config)}
	wanted := make(map[string]bool, len(entries))
	for _, entry := range entries {
		address := listenAddress(entry)
		wanted[address] = true
		if err := r.planOne(ctx, entry, address, &changes); err != nil {
			return listenPlan{}, errors.Join(
				fmt.Errorf("listen on %s: %w", address, err), closeAll(changes.added))
		}
	}
	for address, listener := range r.endpoints {
		if !wanted[address] {
			changes.removed = append(changes.removed, listener)
		}
	}
	return changes, nil
}

// planOne handles one entry and records the result in changes: an address that is already
// open is kept and gets the TLS settings of the entry, any other is opened and added. The
// certificate is loaded before the port is bound, so a bad certificate does not leave a port
// open.
func (r *runner) planOne(
	ctx context.Context, entry config.Endpoint, address string, changes *listenPlan,
) error {
	var tlsConfig *tls.Config // stays nil for a plain listener
	if !entry.Plain {
		var err error
		if tlsConfig, err = tlsConfigFor(entry, r.build.ownCert); err != nil {
			return err
		}
	}
	if listener, ok := r.endpoints[address]; ok {
		changes.kept[listener] = tlsConfig
		return nil
	}
	listener, err := openEndpoint(ctx, address, tlsConfig)
	if err != nil {
		return err
	}
	changes.added = append(changes.added, listener)
	return nil
}
