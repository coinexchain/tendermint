package lite

import (
	"github.com/tendermint/tendermint/lite/types"
)

// Provider provides information for the lite client to sync.
type Provider interface {
	// ChainID returns the blockchain ID.
	ChainID() string

	// LatestFullCommit returns the latest FullCommit.
	//
	// If the provider fails to fetch the FullCommit due to the IO or other
	// issues, an error will be returned.
	LatestFullCommit() (types.FullCommit, error)

	// GetFullCommit returns the FullCommit that corresponds to the given height.
	//
	// If the provider fails to fetch the FullCommit due to the IO or other
	// issues, an error will be returned.
	// If there's no FullCommit for the given height, ErrCommitNotFound will be
	// returned.
	GetFullCommit(height int64) (types.FullCommit, error)
}

// PersistentProvider is a provider that can also persist new information.
type PersistentProvider interface {
	Provider

	// SaveFullCommit saves a FullCommit (without verification).
	SaveFullCommit(fc types.FullCommit) error
}

// UpdatingProvider is a provider that can update itself w/ more recent commit
// data.
//type UpdatingProvider interface {
//	Provider

//	// Update internal information by fetching information somehow.
//	// UpdateToHeight will block until the request is complete, or returns an
//	// error if the request cannot complete. Generally, one must call
//	// UpdateToHeight(h) before GetFullCommit(h) or LatestFullCommit() will
//	// return this height.
//	//
//	// NOTE: behavior with concurrent requests is undefined. To make concurrent
//	// calls safe, look at ConcurrentProvider.
//	UpdateToHeight(height int64) error
//}
