package app

import (
	"errors"
	"fmt"
	"io"

	"github.com/SpliceFS/server/api"
	"github.com/SpliceFS/server/config"
	"github.com/SpliceFS/server/share"
)

// shareAdapter makes a *share.Share an api.Share. Its OpenFile returns a concrete
// *share.File, which cannot satisfy the interface method directly.
type shareAdapter struct {
	*share.Share
}

// Open opens a file of the share.
func (a shareAdapter) Open(name string) (api.File, error) {
	f, err := a.OpenFile(name)
	if err != nil {
		// Returning f itself would wrap a nil pointer in a non-nil interface.
		return nil, err
	}
	return f, nil
}

// shareSet is the open shares of a configuration.
type shareSet struct {
	byName  map[string]api.Share
	closers []io.Closer
}

// openShares opens every share of cfg. If one fails, the ones opened before it are closed.
func openShares(cfg *config.Config) (*shareSet, error) {
	set := &shareSet{byName: make(map[string]api.Share, len(cfg.Shares))}
	for _, name := range cfg.ShareNames() {
		settings := cfg.Shares[name]
		s, err := share.Open(share.Options{
			Name:           name,
			Path:           settings.Path,
			Exclude:        settings.Exclude,
			MinAge:         settings.MinAge.Std(),
			FollowSymlinks: settings.FollowSymlinks,
			MTimePrecision: cfg.MTimePrecision.Std(),
		})
		if err != nil {
			return nil, errors.Join(err, set.Close())
		}
		set.byName[name] = shareAdapter{s}
		set.closers = append(set.closers, s)
	}
	return set, nil
}

// Close closes every share and reports all failures.
func (s *shareSet) Close() error {
	var errs []error
	for _, c := range s.closers {
		if err := c.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close share: %w", err))
		}
	}
	return errors.Join(errs...)
}
