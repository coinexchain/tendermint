package lite

import (
	log "github.com/tendermint/tendermint/libs/log"
	lerr "github.com/tendermint/tendermint/lite/errors"
	"github.com/tendermint/tendermint/lite/types"
)

type hasLogger interface {
	SetLogger(log.Logger)
}

// multiProvider allows you to place one or more caches in front of a source
// Provider.
// It runs through them in order until a match is found.
type multiProvider struct {
	providers []PersistentProvider

	logger log.Logger
}

// NewMultiProvider returns a new provider which wraps multiple other providers.
func NewMultiProvider(providers ...PersistentProvider) PersistentProvider {
	return &multiProvider{
		logger:    log.NewNopLogger(),
		providers: providers,
	}
}

// SetLogger sets logger on self and all subproviders.
func (mc *multiProvider) SetLogger(logger log.Logger) {
	mc.logger = logger
	for _, p := range mc.providers {
		if pp, ok := p.(hasLogger); ok {
			p.SetLogger(logger)
		}
	}
}

// SaveFullCommit saves on all providers, and aborts on the first error.
func (mc *multiProvider) SaveFullCommit(fc types.FullCommit) (err error) {
	for _, p := range mc.providers {
		err = p.SaveFullCommit(fc)
		if err != nil {
			return
		}
	}
	return
}

// LatestFullCommit tries to get latest FullCommit from each provider and
// returns the one with the greatest height.
// Returns the first error encountered.
func (mc *multiProvider) LatestFullCommit() (fc types.FullCommit, err error) {
	for _, p := range mc.providers {
		var pfc types.FullCommit
		pfc, err = p.LatestFullCommit()
		if lerr.IsErrCommitNotFound(err) {
			err = nil
			continue
		} else if err != nil {
			return
		}
		if fc == (types.FullCommit{}) {
			fc = pfc
		} else if pfc.Height() > fc.Height() {
			fc = pfc
		}
	}

	if fc == (types.FullCommit{}) {
		// should not happen usually
		err = lerr.ErrCommitNotFound()
		return
	}

	return
}

// GetFullCommit tries to get FullCommit at height {height} from each provider
// and returns the first found or ErrCommitNotFound if none of the providers
// has it.
// Returns the first error encountered.
func (mc *multiProvider) GetFullCommit(height int64) (fc types.FullCommit, err error) {
	for _, p := range mc.providers {
		var pfc types.FullCommit
		pfc, err = p.GetFullCommit(height)
		if lerr.IsErrCommitNotFound(err) {
			err = nil
			continue
		} else if err != nil {
			return
		}
		return pfc
	}

	if fc == (types.FullCommit{}) {
		err = lerr.ErrCommitNotFound()
		return
	}

	return
}
