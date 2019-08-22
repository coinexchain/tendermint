package lite_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tendermint/tendermint/lite"
	"github.com/tendermint/tendermint/lite/providers/http"
)

func TestExample_Standard(t *testing.T) {
	c, err := lite.NewClient(
		chainID,
		lite.TrustOptions{TrustPeriod: 336 * time.Hour},
		[]string{remote1, remote2},
	)
	require.NoError(t, err)

	commit, err := c.Commit()
	require.NoError(t, err)
	assert.Equal(t, chainID, commit.ChainID)
}

func TestExample_IBC(t *testing.T) {
	sources = []lite.Provider{
		ibc.New(chainID),
	}
	c, err := lite.NewVerifier(
		chainID,
		lite.TrustOptions{TrustPeriod: 24 * time.Hour},
		sources,
		Trusted(ibcKeeper{}),
	)
	require.NoError(t, err)

	err = c.Verify(height)
	require.NoError(t, err)
}
