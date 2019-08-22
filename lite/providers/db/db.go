package db

import (
	"fmt"
	"regexp"
	"strconv"

	amino "github.com/tendermint/go-amino"
	cryptoAmino "github.com/tendermint/tendermint/crypto/encoding/amino"
	log "github.com/tendermint/tendermint/libs/log"
	lerr "github.com/tendermint/tendermint/lite/errors"
	"github.com/tendermint/tendermint/types"
	dbm "github.com/tendermint/tm-db"
)

// DB provider stores commits and validator sets in a DB.
//
// The number of heights for which DB stores commits and validator sets
// can be optionally limited by calling SetLimit with the desired limit.
type DB struct {
	chainID string
	db      dbm.DB
	cdc     *amino.Codec
	limit   int

	logger log.Logger
}

// New returns a DB provider.
func New(chainID string, db dbm.DB) *DBProvider {
	// NOTE: when debugging, this type of construction might be useful.
	// db = dbm.NewDebugDB("db provider "+cmn.RandStr(4), db)

	cdc := amino.NewCodec()
	cryptoAmino.RegisterAmino(cdc)
	dbp := &DB{
		logger:  log.NewNopLogger(),
		chainID: chainID,
		db:      db,
		cdc:     cdc,
	}
	return dbp
}

// SetLimit limits the number of heights for which DB stores commits
// and validator sets. E.g. 3 will result in storing only commits and validator
// sets for the 3 latest heights.
func (dbp *DB) SetLimit(limit int) *DB {
	dbp.limit = limit
	return dbp
}

// SetLogger sets the logger.
func (dbp *DB) SetLogger(logger log.Logger) {
	dbp.logger = logger
}

func (dbp *DB) SaveFullCommit(fc FullCommit) error {
	dbp.logger.Info("DB.SaveFullCommit()...", "fc", fc)

	batch := dbp.db.NewBatch()
	defer batch.Close()

	// Save the fc.validators.
	// We might be overwriting what we already have, but
	// it makes the logic easier for now.
	vsKey := validatorSetKey(fc.ChainID(), fc.Height())
	vsBz, err := dbp.cdc.MarshalBinaryLengthPrefixed(fc.Validators)
	if err != nil {
		return err
	}
	batch.Set(vsKey, vsBz)

	// Save the fc.NextValidators.
	nvsKey := validatorSetKey(fc.ChainID(), fc.Height()+1)
	nvsBz, err := dbp.cdc.MarshalBinaryLengthPrefixed(fc.NextValidators)
	if err != nil {
		return err
	}
	batch.Set(nvsKey, nvsBz)

	// Save the fc.SignedHeader.
	shKey := signedHeaderKey(fc.ChainID(), fc.Height())
	shBz, err := dbp.cdc.MarshalBinaryLengthPrefixed(fc.SignedHeader)
	if err != nil {
		return err
	}
	batch.Set(shKey, shBz)

	// And write sync.
	batch.WriteSync()

	// Garbage collect.
	// TODO: optimize later.
	if dbp.limit > 0 {
		dbp.deleteAfterN(fc.ChainID(), dbp.limit)
	}

	return nil
}

func (dbp *DB) LatestFullCommit() (FullCommit, error) {
	dbp.logger.Info("DB.LatestFullCommit()...")

	itr := dbp.db.ReverseIterator(
		signedHeaderKey(chainID, 1),
		append(signedHeaderKey(chainID, 1<<63-1), byte(0x00)),
	)
	defer itr.Close()

	for itr.Valid() {
		key := itr.Key()
		_, _, ok := parseSignedHeaderKey(key)
		if !ok {
			// Skip over other keys.
			itr.Next()
			continue
		} else {
			// Found the latest full commit signed header.
			shBz := itr.Value()
			sh := types.SignedHeader{}
			err := dbp.cdc.UnmarshalBinaryLengthPrefixed(shBz, &sh)
			if err != nil {
				return FullCommit{}, err
			}
			lfc, err := dbp.fillFullCommit(sh)
			if err == nil {
				dbp.logger.Info("DB.LatestFullCommit() found latest", "height", lfc.Height())
				return lfc, nil
			}
			dbp.logger.Error("DB.LatestFullCommit() got error", "lfc", lfc, "err", err)
			return lfc, err
		}
	}

	return FullCommit{}, lerr.ErrCommitNotFound()
}

func (dbp *DB) GetFullCommit(height int64) (FullCommit, error) {
	dbp.logger.Info("DB.GetFullCommit()...", "height", height)

	shBz := dbp.db.Get(signedHeaderKey(chainID, height))
	if shBz == nil {
		return FullCommit{}, lerr.ErrCommitNotFound()
	}

	sh := types.SignedHeader{}
	err := dbp.cdc.UnmarshalBinaryLengthPrefixed(shBz, &sh)
	if err != nil {
		return FullCommit{}, err
	}
	lfc, err := dbp.fillFullCommit(sh)
	if err == nil {
		dbp.logger.Info("DB.GetFullCommit() found commit", "height", lfc.Height())
		return lfc, nil
	}

	dbp.logger.Error("DB.GetFullCommit() got error", "lfc", lfc, "err", err)
	return lfc, err
}

func (dbp *DB) getValidatorSet(chainID string, height int64) (valset *types.ValidatorSet, err error) {
	vsBz := dbp.db.Get(validatorSetKey(chainID, height))
	if vsBz == nil {
		err = lerr.ErrUnknownValidators(chainID, height)
		return
	}
	err = dbp.cdc.UnmarshalBinaryLengthPrefixed(vsBz, &valset)
	if err != nil {
		return
	}

	// To test deep equality.  This makes it easier to test for e.g. valset
	// equivalence using assert.Equal (tests for deep equality) in our tests,
	// which also tests for unexported/private field equivalence.
	valset.TotalVotingPower()

	return
}

func (dbp *DB) fillFullCommit(sh types.SignedHeader) (FullCommit, error) {
	var (
		chainID = sh.ChainID
		height  = sh.Height
	)

	// Load the validator set.
	valset, err := dbp.getValidatorSet(chainID, height)
	if err != nil {
		return FullCommit{}, err
	}

	// Load the next validator set.
	nextValset, err := dbp.getValidatorSet(chainID, height+1)
	if err != nil {
		return FullCommit{}, err
	}

	// Return filled FullCommit.
	return FullCommit{
		SignedHeader:   sh,
		Validators:     valset,
		NextValidators: nextValset,
	}, nil
}

// deleteAfterN deletes all items except skipping first {after} items.
// example - deleteAfterN("test", 1):
//   - signedHeader#188
//   - signedHeader#187
//   - validatorSet#187
//   - signedHeader#186
// ==>
//   - signedHeader#188
func (dbp *DB) deleteAfterN(chainID string, after int) error {
	dbp.logger.Debug("DB.deleteAfterN()...", "chainID", chainID, "after", after)

	itr := dbp.db.ReverseIterator(
		signedHeaderKey(chainID, 1),
		append(signedHeaderKey(chainID, 1<<63-1), byte(0x00)),
	)
	defer itr.Close()

	var (
		minHeight  int64 = 1<<63 - 1
		numSeen          = 0
		numDeleted       = 0
	)

	for itr.Valid() {
		key := itr.Key()
		_, height, ok := parseChainKeyPrefix(key)
		if !ok {
			return fmt.Errorf("unexpected key %v", key)
		}
		if height < minHeight {
			minHeight = height
			numSeen++
		}
		if numSeen > after {
			dbp.db.Delete(key)
			numDeleted++
		}
		itr.Next()
	}

	dbp.logger.Debug(fmt.Sprintf("DB.deleteAfterN() deleted %d items (seen %d)", numDeleted, numSeen))
	return nil
}

//----------------------------------------
// key encoding

func signedHeaderKey(chainID string, height int64) []byte {
	return []byte(fmt.Sprintf("%s/%010d/sh", chainID, height))
}

func validatorSetKey(chainID string, height int64) []byte {
	return []byte(fmt.Sprintf("%s/%010d/vs", chainID, height))
}

//----------------------------------------
// key parsing

var keyPattern = regexp.MustCompile(`^([^/]+)/([0-9]*)/(.*)$`)

func parseKey(key []byte) (chainID string, height int64, part string, ok bool) {
	submatch := keyPattern.FindSubmatch(key)
	if submatch == nil {
		return "", 0, "", false
	}
	chainID = string(submatch[1])
	heightStr := string(submatch[2])
	heightInt, err := strconv.Atoi(heightStr)
	if err != nil {
		return "", 0, "", false
	}
	height = int64(heightInt)
	part = string(submatch[3])
	ok = true // good!
	return
}

func parseSignedHeaderKey(key []byte) (chainID string, height int64, ok bool) {
	var part string
	chainID, height, part, ok = parseKey(key)
	if part != "sh" {
		return "", 0, false
	}
	return
}

func parseChainKeyPrefix(key []byte) (chainID string, height int64, ok bool) {
	chainID, height, _, ok = parseKey(key)
	return
}
