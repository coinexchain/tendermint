package providers

import (
	"fmt"

	log "github.com/tendermint/tendermint/libs/log"
	lerr "github.com/tendermint/tendermint/lite/errors"
	"github.com/tendermint/tendermint/lite/types"
	rpcclient "github.com/tendermint/tendermint/rpc/client"
	ctypes "github.com/tendermint/tendermint/types"
)

// SignStatusClient combines a SignClient and StatusClient.
type SignStatusClient interface {
	rpcclient.SignClient
	rpcclient.StatusClient
}

// HTTP provider uses an RPC client (or SignStatusClient more generally) to
// obtain the necessary information.
type HTTP struct {
	chainID string
	client  SignStatusClient

	logger log.Logger
}

// NewHTTP creates a HTTP provider, which is using the rpcclient.HTTP
// client under the hood.
func NewHTTP(chainID, remote string) *HTTP {
	return NewHTTPWithClient(chainID, rpcclient.NewHTTP(remote, "/websocket"))
}

// NewHTTPWithClient allows you to provide custom SignStatusClient.
func NewHTTPWithClient(chainID string, client SignStatusClient) *HTTP {
	return &provider{
		chainID: chainID,
		client:  client,
		logger:  log.NewNopLogger(),
	}
}

// SetLogger sets the logger.
func (p *HTTP) SetLogger(logger log.Logger) {
	p.logger = logger
}

func (p *HTTP) LatestFullCommit() (fc types.FullCommit, err error) {
	return p.GetFullCommit(0)
}

func (p *HTTP) GetFullCommit(height int64) (fc types.FullCommit, err error) {
	commit, err := p.client.Commit(height)
	if err != nil {
		return fc, err
	}

	// Verify we're still on the same chain.
	if p.chainID != commit.Header.ChainID {
		return fc, fmt.Errorf("expected chainID %s, got %s", p.chainID, commit.Header.ChainID)
	}

	return p.fillFullCommit(commit.SignedHeader)
}

func (p *HTTP) fillFullCommit(signedHeader ctypes.SignedHeader) (fc types.FullCommit, err error) {
	// Get the validators.
	valset, err := p.getValidatorSet(signedHeader.ChainID, signedHeader.Height)
	if err != nil {
		return types.FullCommit{}, err
	}

	// Get the next validators.
	nextValset, err := p.getValidatorSet(signedHeader.ChainID, signedHeader.Height+1)
	if err != nil {
		return types.FullCommit{}, err
	}

	return types.NewFullCommit(signedHeader, valset, nextValset), nil
}

func (p *HTTP) getValidatorSet(chainID string, height int64) (valset *ctypes.ValidatorSet, err error) {
	if height < 1 {
		return nil, fmt.Errorf("expected height >= 1, got height %d", height)
	}

	res, err := p.client.Validators(&height)
	if err != nil {
		// TODO pass through other types of errors.
		return nil, lerr.ErrUnknownValidators(height)
	}

	return types.NewValidatorSet(res.Validators), nil
}
