package lite

import (
	"testing"
	"time"

	lclient "github.com/tendermint/tendermint/lite/client"
	"github.com/tendermint/tendermint/lite/verifying"
)

func TestExample_MinimalSetup(t *testing.T) {
	// at least two sources is needed for security
	sources = []Provider{
		lclient.NewHTTPProvider(chainID, remote1),
		lclient.NewHTTPProvider(chainID, remote2),
	}
	v, err := NewVerifier(
		chainID,
		TrustOptions{TrustPeriod: 336 * time.Hour},
		sources,
	)
	require.NoError(t, err)

	err = v.Verify(newHeader)
	require.NoError(t, err)
}
