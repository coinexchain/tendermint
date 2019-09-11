package lite

import (
	"bytes"
	"fmt"
	"math"
	"time"

	"github.com/pkg/errors"

	log "github.com/tendermint/tendermint/libs/log"
	"github.com/tendermint/tendermint/lite"
	lclient "github.com/tendermint/tendermint/lite/client"
	lerr "github.com/tendermint/tendermint/lite/errors"
	"github.com/tendermint/tendermint/lite/providers/db"
	"github.com/tendermint/tendermint/types"
	dbm "github.com/tendermint/tm-db"
)

const (
	loggerPath = "lite"
	memDBFile  = "trusted.mem"
	cacheSize  = 100
	lvlDBFile  = "trusted.lvl"
	dbName     = "trust-base"
)

// TrustOptions are the trust parameters needed for when a new light client
// connects to the network or when a light client that has been offline for
// longer than the unbonding period connects to the network.
//
// The expectation is the user will get this information from a trusted source
// like a validator, a friend, or a secure website. A more user friendly
// solution with trust tradeoffs is that we establish an https based protocol
// with a default end point that populates this information. Also an on-chain
// registry of roots-of-trust (e.g. on the Cosmos Hub) seems likely in the
// future.
type TrustOptions struct {
	// Required: only trust commits up to this old.
	// Should be equal to the unbonding period minus a configurable evidence
	// submission synchrony bound.
	TrustPeriod time.Duration `json:"trust-period"`

	// Option 1: TrustHeight and TrustHash can both be provided
	// to force the trusting of a particular height and hash.
	// If the latest trusted height/hash is more recent, then this option is
	// ignored.
	TrustHeight int64  `json:"trust-height"`
	TrustHash   []byte `json:"trust-hash"`

	// Option 2: Callback can be set to implement a confirmation
	// step if the trust store is uninitialized, or expired.
	Callback func(height int64, hash []byte) error
}

// Option1 returns true if TrustHeight and TrustHash are present.
func (opts TrustOptions) Option1() bool {
	return opts.TrustHeight > 0 && len(opts.TrustHash) > 0
}

type mode int

const (
	sequential mode = iota
	bisecting
)

// SequentialVerification option can be used to instruct Verifier to
// sequentially check the headers. Note this is much slower than
// BisectingVerification, albeit more secure.
func SequentialVerification() Option {
	return func(v *Verifier) {
		v.mode = sequential
	}
}

// BisectingVerification option can be used to instruct Verifier to check the
// headers using bisection algorithm described in XXX.
//
// trustLevel - maximum change between two not consequitive headers in terms of
// validators & their respective voting power, required to trust a new header
// (default: 1/3).
func BisectingVerification(trustLevel float) Option {
	if trustLevel > 1 || trustLevel < 1/3 {
		panic(fmt.Sprintf("trustLevel must be within [1/3, 1], given %v", trustLevel))
	}
	return func(v *Verifier) {
		v.mode = bisecting
		v.trustLevel = trustLevel
	}
}

// DefaultBisectingVerification is BisectingVerification option with
// trustLevel=1/3.
var DefaultBisectingVerification = func() Option {
	return BisectingVerification(1 / 3)
}

// Trusted option can be used to change default trusted provider. See
// NewVerifier func.
func Trusted(trusted Provider) Option {
	return func(v *Verifier) {
		v.mode = bisecting
	}
}

// AlternativeSources option can be used to supply alternative sources, which
// will be used for cross-checking the primary source of new headers.
func AlternativeSources(sources []Provider) Option {
	return func(v *Verifier) {
		v.alternativeSources = sources
	}
}

// Verifier is the core of the light client performing validation of the
// headers it obtains from the source provider. It stores properly validated
// data on the "trusted" local system using a "trusted" Provider.
//
// It periodically cross-validates the source provider by checking alternative
// sources (optional).
type Verifier struct {
	chainID            string
	trustOptions       TrustOptions
	mode               mode
	trustLevel         float
	lastVerifiedHeight int64

	// Source of new headers.
	source Provider

	// Alternative sources for checking the primary for misbehavior by comparing
	// data.
	alternativeSources []Provider

	// Where trusted headers are stored.
	trusted PersistentProvider

	logger log.Logger
}

// NewVerifier returns a new Verifier.
//
// If no trusted provider is configured using Trusted option, MultiProvider
// will be used (in-memory cache with capacity=100 in front of goleveldb
// database).
func NewVerifier(chainID string, trustOptions TrustOptions, source Provider,
	options *Option) *Verifier {

	v := Verifier{
		chainID:      chainID,
		trustOptions: trustOptions,
		source:       source,
	}

	for _, o := range options {
		o(vp)
	}

	// Better to execute after to avoid unnecessary initialization.
	if v.trusted == nil {
		v.trusted = NewMultiProvider(
			db.New(memDBFile, dbm.NewMemDB()).SetLimit(cacheSize),
			db.New(lvlDBFile, dbm.NewDB(dbName, dbm.GoLevelDBBackend, rootDir)),
		)
	}
}

// NewProvider creates a Provider.
//
// NOTE: If you retain the resulting struct in memory for a long time, usage of
// it may eventually error, but immediate usage should not error like that, so
// that e.g. cli usage never errors unexpectedly.
func NewProvider(chainID, rootDir string, client lclient.SignStatusClient,
	logger log.Logger, cacheSize int, options TrustOptions) (*Provider, error) {

	vp := initProvider(chainID, rootDir, client, logger, cacheSize, options)

	// Get the latest source commit, or the one provided in options.
	trustCommit, err := getTrustedCommit(vp.logger, client, options)
	if err != nil {
		return nil, err
	}

	err = vp.fillValsetAndSaveFC(trustCommit, nil, nil)
	if err != nil {
		return nil, err
	}

	// sanity check
	// FIXME: Can't it happen that the local clock is a bit off and the
	// trustCommit.Time is a few seconds in the future?
	now := time.Now()
	if now.Sub(trustCommit.Time) <= 0 {
		panic(fmt.Sprintf("impossible time %v vs %v", now, trustCommit.Time))
	}

	// Otherwise we're syncing within the unbonding period.
	// NOTE: There is a duplication of fetching this latest commit (since
	// UpdateToHeight() will fetch it again, and latestCommit isn't used), but
	// it's only once upon initialization so it's not a big deal.
	if options.Option1() {
		// Fetch latest commit (nil means latest height).
		latestCommit, err := client.Commit(nil)
		if err != nil {
			return nil, err
		}
		err = vp.UpdateToHeight(chainID, latestCommit.SignedHeader.Height)
		if err != nil {
			return nil, err
		}
	}

	return vp, nil
}

func initProvider(chainID, rootDir string, client lclient.SignStatusClient,
	logger log.Logger, cacheSize int, options TrustOptions) *Provider {

	// Validate TrustOptions.
	if options.TrustPeriod == 0 {
		panic("Provider must have non-zero trust period")
	}

	// Init logger.
	logger = logger.With("module", loggerPath)
	logger.Info("lite/verifying/NewProvider", "chainID", chainID, "rootDir", rootDir, "client", client)

	// The trusted Provider should be a DBProvider.
	trusted := lite.NewMultiProvider(
		lite.NewDBProvider(memDBFile, dbm.NewMemDB()).SetLimit(cacheSize),
		lite.NewDBProvider(lvlDBFile, dbm.NewDB(dbName, dbm.GoLevelDBBackend, rootDir)),
	)
	trusted.SetLogger(logger)

	// The source Provider should be a client.HTTPProvider.
	source := lclient.NewProvider(chainID, client)
	source.SetLogger(logger)

	return &Provider{
		chainID:              chainID,
		trustPeriod:          options.TrustPeriod,
		trusted:              trusted,
		source:               source,
		logger:               logger,
		pendingVerifications: make(map[int64]chan struct{}, sizeOfPendingMap),
	}
}

// getTrustedCommit returns a commit trusted with weak subjectivity. It either:
// 1. Fetches a commit at height provided in options and ensures the specified
// commit is within the trust period of latest block
// 2. Trusts the remote node and gets the latest commit
// 3. Returns an error if the height provided in trust option is too old to
// sync to latest.
func getTrustedCommit(logger log.Logger, client lclient.SignStatusClient, options TrustOptions) (types.SignedHeader, error) {
	// Get the latest commit always.
	latestCommit, err := client.Commit(nil)
	if err != nil {
		return types.SignedHeader{}, err
	}

	// If the user has set a root of trust, confirm it then update to newest.
	if options.Option1() {
		trustCommit, err := client.Commit(&options.TrustHeight)
		if err != nil {
			return types.SignedHeader{}, err
		}

		if latestCommit.Time.Sub(trustCommit.Time) > options.TrustPeriod {
			return types.SignedHeader{},
				errors.New("your trusted block height is older than the trust period from latest block")
		}

		signedHeader := trustCommit.SignedHeader
		if !bytes.Equal(signedHeader.Hash(), options.TrustHash) {
			return types.SignedHeader{},
				fmt.Errorf("WARNING: expected hash %X, but got %X", options.TrustHash, signedHeader.Hash())
		}
		return signedHeader, nil
	}

	signedHeader := latestCommit.SignedHeader

	// NOTE: This should really belong in the callback.
	// WARN THE USER IN ALL CAPS THAT THE LITE CLIENT IS NEW, AND THAT WE WILL
	// SYNC TO AND VERIFY LATEST COMMIT.
	logger.Info("WARNING: trusting source at height %d and hash %X...\n", signedHeader.Height, signedHeader.Hash())
	if options.Callback != nil {
		err := options.Callback(signedHeader.Height, signedHeader.Hash())
		if err != nil {
			return types.SignedHeader{}, err
		}
	}
	return signedHeader, nil
}

func (vp *Provider) Verify(signedHeader types.SignedHeader) error {
	if signedHeader.ChainID != vp.chainID {
		return fmt.Errorf("expected chainID %s, got %s", vp.chainID, signedHeader.ChainID)
	}

	valSet, err := vp.ValidatorSet(signedHeader.ChainID, signedHeader.Height)
	if err != nil {
		return err
	}

	if signedHeader.Height < vp.height {
		return fmt.Errorf("expected height %d, got %d", vp.height, signedHeader.Height)
	}

	if !bytes.Equal(signedHeader.ValidatorsHash, valSet.Hash()) {
		return lerr.ErrUnexpectedValidators(signedHeader.ValidatorsHash, valSet.Hash())
	}

	err = signedHeader.ValidateBasic(vp.chainID)
	if err != nil {
		return err
	}

	// Check commit signatures.
	err = valSet.VerifyCommit(vp.chainID, signedHeader.Commit.BlockID, signedHeader.Height, signedHeader.Commit)
	if err != nil {
		return err
	}

	return nil
}

func (vp *Provider) SetLogger(logger log.Logger) {
	vp.logger = logger
	vp.trusted.SetLogger(logger)
	vp.source.SetLogger(logger)
}

func (vp *Provider) ChainID() string { return vp.chainID }

// UpdateToHeight ... stores the full commit (SignedHeader + Validators) in
// vp.trusted.
func (vp *Provider) UpdateToHeight(chainID string, height int64) error {
	_, err := vp.trusted.LatestFullCommit(vp.chainID, height, height)
	// If we alreedy have the commit, just return nil.
	if err == nil {
		return nil
	} else if !lerr.IsErrCommitNotFound(err) {
		// Return error if it is not CommitNotFound error.
		vp.logger.Error("Encountered unknown error while loading full commit", "height", height, "err", err)
		return err
	}

	// Fetch trusted FC at exactly height, while updating trust when possible.
	_, err = vp.fetchAndVerifyToHeightBisecting(height)
	if err != nil {
		return err
	}

	vp.height = height

	// Good!
	return nil
}

// If valset or nextValset are nil, fetches them.
// Then validates full commit, then saves it.
func (vp *Provider) fillValsetAndSaveFC(signedHeader types.SignedHeader,
	valset, nextValset *types.ValidatorSet) (err error) {

	// If there is no valset passed, fetch it
	if valset == nil {
		valset, err = vp.source.ValidatorSet(vp.chainID, signedHeader.Height)
		if err != nil {
			return errors.Wrap(err, "fetching the valset")
		}
	}

	// If there is no nextvalset passed, fetch it
	if nextValset == nil {
		// TODO: Don't loop forever, just do it 10 times
		for {
			// fetch block at signedHeader.Height+1
			nextValset, err = vp.source.ValidatorSet(vp.chainID, signedHeader.Height+1)
			if lerr.IsErrValidatorSetNotFound(err) {
				// try again until we get it.
				vp.logger.Debug("fetching valset for height %d...\n", signedHeader.Height+1)
				continue
			} else if err != nil {
				return errors.Wrap(err, "fetching the next valset")
			} else if nextValset != nil {
				break
			}
		}
	}

	// Create filled FullCommit.
	fc := lite.FullCommit{
		SignedHeader:   signedHeader,
		Validators:     valset,
		NextValidators: nextValset,
	}

	// Validate the full commit.  This checks the cryptographic
	// signatures of Commit against Validators.
	if err := fc.ValidateFull(vp.chainID); err != nil {
		return errors.Wrap(err, "verifying validators from source")
	}

	// Trust it.
	err = vp.trusted.SaveFullCommit(fc)
	if err != nil {
		return errors.Wrap(err, "saving full commit")
	}

	return nil
}

// verifyAndSave will verify if this is a valid source full commit given the
// best match trusted full commit, and persist to vp.trusted.
//
// Returns ErrTooMuchChange when >2/3 of trustedFC did not sign newFC.
// Returns ErrCommitExpired when trustedFC is too old.
// Panics if trustedFC.Height() >= newFC.Height().
func (vp *Provider) verifyAndSave(trustedFC, newFC lite.FullCommit) error {
	// Shouldn't have trusted commits before the new commit height.
	if trustedFC.Height() >= newFC.Height() {
		panic("should not happen")
	}

	// Check that the latest commit isn't beyond the vp.trustPeriod.
	if vp.now().Sub(trustedFC.SignedHeader.Time) > vp.trustPeriod {
		return lerr.ErrCommitExpired()
	}

	// Validate the new commit in terms of validator set of last trusted commit.
	if err := trustedFC.NextValidators.VerifyCommit(vp.chainID, newFC.SignedHeader.Commit.BlockID, newFC.SignedHeader.Height, newFC.SignedHeader.Commit); err != nil {
		return err
	}

	// Locally validate the full commit before we can trust it.
	if newFC.Height() >= trustedFC.Height()+1 {
		err := newFC.ValidateFull(vp.chainID)

		if err != nil {
			return err
		}
	}

	change := compareVotingPowers(trustedFC, newFC)
	if change > float64(1/3) {
		return lerr.ErrValidatorChange(change)
	}

	return vp.trusted.SaveFullCommit(newFC)
}

func compareVotingPowers(trustedFC, newFC lite.FullCommit) float64 {
	var diffAccumulator float64

	for _, val := range newFC.Validators.Validators {
		newPowerRatio := float64(val.VotingPower) / float64(newFC.Validators.TotalVotingPower())
		_, tval := trustedFC.NextValidators.GetByAddress(val.Address)
		oldPowerRatio := float64(tval.VotingPower) / float64(trustedFC.NextValidators.TotalVotingPower())
		diffAccumulator += math.Abs(newPowerRatio - oldPowerRatio)
	}

	return diffAccumulator
}

func (vp *Provider) fetchAndVerifyToHeightLinear(h int64) (lite.FullCommit, error) {
	// Fetch latest full commit from source.
	sourceFC, err := vp.source.LatestFullCommit(vp.chainID, h, h)
	if err != nil {
		return lite.FullCommit{}, err
	}

	// If sourceFC.Height() != h, we can't do it.
	if sourceFC.Height() != h {
		return lite.FullCommit{}, lerr.ErrCommitNotFound()
	}

	// Validate the full commit.  This checks the cryptographic
	// signatures of Commit against Validators.
	if err := sourceFC.ValidateFull(vp.chainID); err != nil {
		return lite.FullCommit{}, err
	}

	if h == sourceFC.Height()+1 {
		trustedFC, err := vp.trusted.LatestFullCommit(vp.chainID, 1, h)
		if err != nil {
			return lite.FullCommit{}, err
		}

		err = vp.verifyAndSave(trustedFC, sourceFC)

		if err != nil {
			return lite.FullCommit{}, err
		}
		return sourceFC, nil
	}

	// Verify latest FullCommit against trusted FullCommits
	// Use a loop rather than recursion to avoid stack overflows.
	for {
		// Fetch latest full commit from trusted.
		trustedFC, err := vp.trusted.LatestFullCommit(vp.chainID, 1, h)
		if err != nil {
			return lite.FullCommit{}, err
		}

		// We have nothing to do.
		if trustedFC.Height() == h {
			return trustedFC, nil
		}
		sourceFC, err = vp.source.LatestFullCommit(vp.chainID, trustedFC.Height()+1, trustedFC.Height()+1)

		if err != nil {
			return lite.FullCommit{}, err
		}
		err = vp.verifyAndSave(trustedFC, sourceFC)

		if err != nil {
			return lite.FullCommit{}, err
		}
	}
}

// fetchAndVerifyToHeightBiscecting will use divide-and-conquer to find a path to h.
// Returns nil error iff we successfully verify for height h, using repeated
// applications of bisection if necessary.
// Along the way, if a recent trust is used to verify a more recent header, the
// more recent header becomes trusted.
//
// Returns ErrCommitNotFound if source Provider doesn't have the commit for h.
func (vp *Provider) fetchAndVerifyToHeightBisecting(h int64) (lite.FullCommit, error) {
	// Fetch latest full commit from source.
	sourceFC, err := vp.source.LatestFullCommit(vp.chainID, h, h)
	if err != nil {
		return lite.FullCommit{}, err
	}

	// If sourceFC.Height() != h, we can't do it.
	if sourceFC.Height() != h {
		return lite.FullCommit{}, lerr.ErrCommitNotFound()
	}

	// Validate the full commit.  This checks the cryptographic
	// signatures of Commit against Validators.
	if err := sourceFC.ValidateFull(vp.chainID); err != nil {
		return lite.FullCommit{}, err
	}

	// Verify latest FullCommit against trusted FullCommits
	// Use a loop rather than recursion to avoid stack overflows.
	for {
		// Fetch latest full commit from trusted.
		trustedFC, err := vp.trusted.LatestFullCommit(vp.chainID, 1, h)
		if err != nil {
			return lite.FullCommit{}, err
		}

		// We have nothing to do.
		if trustedFC.Height() == h {
			return trustedFC, nil
		}

		// Update to full commit with checks.
		err = vp.verifyAndSave(trustedFC, sourceFC)

		// Handle special case when err is ErrTooMuchChange.
		if types.IsErrTooMuchChange(err) {
			// Divide and conquer.
			start, end := trustedFC.Height(), sourceFC.Height()
			if !(start < end) {
				panic("should not happen")
			}
			mid := (start + end) / 2

			// Recursive call back into fetchAndVerifyToHeight. Once you get to an inner
			// call that succeeeds, the outer calls will succeed.
			_, err = vp.fetchAndVerifyToHeightBisecting(mid)
			if err != nil {
				return lite.FullCommit{}, err
			}
			// If we made it to mid, we retry.
			continue
		} else if err != nil {
			return lite.FullCommit{}, err
		}

		// All good!
		return sourceFC, nil
	}
}

func (vp *Provider) LastTrustedHeight() int64 {
	fc, err := vp.trusted.LatestFullCommit(vp.chainID, 1, 1<<63-1)
	if err != nil {
		panic("should not happen")
	}
	return fc.Height()
}

func (vp *Provider) LatestFullCommit(chainID string, minHeight, maxHeight int64) (lite.FullCommit, error) {
	return vp.trusted.LatestFullCommit(chainID, minHeight, maxHeight)
}

func (vp *Provider) ValidatorSet(chainID string, height int64) (*types.ValidatorSet, error) {
	// XXX try to sync?
	return vp.trusted.ValidatorSet(chainID, height)
}
