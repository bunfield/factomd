// Copyright (c) 2013 Conformal Systems LLC.
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package btcchain

import (
	"encoding/binary"
	"fmt"
	"github.com/conformal/btcdb"
	"github.com/conformal/btcscript"
	"github.com/conformal/btcutil"
	"github.com/conformal/btcwire"
	"math"
	"time"
)

const (
	// satoshiPerBitcoin is the number of satoshi in one bitcoin (1 BTC).
	satoshiPerBitcoin int64 = 1e8

	// maxSatoshi is the maximum transaction amount allowed in satoshi.
	maxSatoshi int64 = 21e6 * satoshiPerBitcoin

	// maxSigOpsPerBlock is the maximum number of signature operations
	// allowed for a block.  It is a fraction of the max block payload size.
	maxSigOpsPerBlock = btcwire.MaxBlockPayload / 50

	// lockTimeThreshold is the number below which a lock time is
	// interpreted to be a block number.  Since an average of one block
	// is generated per 10 minutes, this allows blocks for about 9,512
	// years.  However, if the field is interpreted as a timestamp, given
	// the lock time is a uint32, the max is sometime around 2106.
	lockTimeThreshold uint32 = 5e8 // Tue Nov 5 00:53:20 1985 UTC

	// minCoinbaseScriptLen is the minimum length a coinbase script can be.
	minCoinbaseScriptLen = 2

	// maxCoinbaseScriptLen is the maximum length a coinbase script can be.
	maxCoinbaseScriptLen = 100

	// medianTimeBlocks is the number of previous blocks which should be
	// used to calculate the median time used to validate block timestamps.
	medianTimeBlocks = 11

	// serializedHeightVersion is the block version which changed block
	// coinbases to start with the serialized block height.
	serializedHeightVersion = 2

	// baseSubsidy is the starting subsidy amount for mined blocks.  This
	// value is halved every subsidyHalvingInterval blocks.
	baseSubsidy = 50 * satoshiPerBitcoin

	// subsidyHalvingInterval is the interval of blocks at which the
	// baseSubsidy is continually halved.  See calcBlockSubsidy for more
	// details.
	subsidyHalvingInterval = 210000
)

var (
	// coinbaseMaturity is the number of blocks required before newly
	// mined bitcoins (coinbase transactions) can be spent.  This is a
	// variable as opposed to a constant because the tests need the ability
	// to modify it.
	coinbaseMaturity int64 = 100

	// zeroHash is the zero value for a btcwire.ShaHash and is defined as
	// a package level variable to avoid the need to create a new instance
	// every time a check is needed.
	zeroHash = &btcwire.ShaHash{}

	// block91842Hash is one of the two nodes which violate the rules
	// set forth in BIP0030.  It is defined as a package level variable to
	// avoid the need to create a new instance every time a check is needed.
	block91842Hash = newShaHashFromStr("00000000000a4d0a398161ffc163c503763b1f4360639393e0e4c8e300e0caec")

	// block91880Hash is one of the two nodes which violate the rules
	// set forth in BIP0030.  It is defined as a package level variable to
	// avoid the need to create a new instance every time a check is needed.
	block91880Hash = newShaHashFromStr("00000000000743f190a18c5577a3c2d2a1f610ae9601ac046a38084ccb7cd721")
)

// isNullOutpoint determines whether or not a previous transaction output point
// is set.
func isNullOutpoint(outpoint *btcwire.OutPoint) bool {
	if outpoint.Index == math.MaxUint32 && outpoint.Hash.IsEqual(zeroHash) {
		return true
	}
	return false
}

// isCoinBase determines whether or not a transaction is a coinbase.  A coinbase
// is a special transaction created by miners that has no inputs.  This is
// represented in the block chain by a transaction with a single input that has
// a previous output transaction index set to the maximum value along with a
// zero hash.
func isCoinBase(msgTx *btcwire.MsgTx) bool {
	// A coin base must only have one transaction input.
	if len(msgTx.TxIn) != 1 {
		return false
	}

	// The previous output of a coin base must have a max value index and
	// a zero hash.
	prevOut := msgTx.TxIn[0].PreviousOutpoint
	if prevOut.Index != math.MaxUint32 || !prevOut.Hash.IsEqual(zeroHash) {
		return false
	}

	return true
}

// isFinalized determines whether or not a transaction is finalized.
func isFinalizedTransaction(msgTx *btcwire.MsgTx, blockHeight int64, blockTime time.Time) bool {
	// Lock time of zero means the transaction is finalized.
	lockTime := msgTx.LockTime
	if lockTime == 0 {
		return true
	}

	// The lock time field of a transaction is either a block height at
	// which the transaction is finalized or a timestamp depending on if the
	// value is before the lockTimeThreshold.  When it is under the
	// threshold it is a block height.
	blockTimeOrHeight := int64(0)
	if lockTime < lockTimeThreshold {
		blockTimeOrHeight = blockHeight
	} else {
		blockTimeOrHeight = blockTime.Unix()
	}
	if int64(lockTime) < blockTimeOrHeight {
		return true
	}

	// At this point, the transaction's lock time hasn't occured yet, but
	// the transaction might still be finalized if the sequence number
	// for all transaction inputs is maxed out.
	for _, txIn := range msgTx.TxIn {
		if txIn.Sequence != math.MaxUint32 {
			return false
		}
	}
	return true
}

// isBIP0030Node returns whether or not the passed node represents one of the
// two blocks that violate the BIP0030 rule which prevents transactions from
// overwriting old ones.
func isBIP0030Node(node *blockNode) bool {
	if node.height == 91842 && node.hash.IsEqual(block91842Hash) {
		return true
	}

	if node.height == 91880 && node.hash.IsEqual(block91880Hash) {
		return true
	}

	return false
}

// calcBlockSubsidy returns the subsidy amount a block at the provided height
// should have. This is mainly used for determining how much the coinbase for
// newly generated blocks awards as well as validating the coinbase for blocks
// has the expected value.
//
// The subsidy is halved every subsidyHalvingInterval blocks.  Mathematically
// this is: baseSubsidy / 2^(height/subsidyHalvingInterval)
//
// At the target block generation rate this is approximately every 4
// years.
func calcBlockSubsidy(height int64) int64 {
	// Equivalent to: baseSubsidy / 2^(height/subsidyHalvingInterval)
	return baseSubsidy >> uint(height/subsidyHalvingInterval)
}

// checkTransactionSanity performs some preliminary checks on a transaction to
// ensure it is sane.  These checks are context free.
func checkTransactionSanity(tx *btcwire.MsgTx) error {
	// A transaction must have at least one input.
	if len(tx.TxIn) == 0 {
		return RuleError("transaction has no inputs")
	}

	// A transaction must have at least one output.
	if len(tx.TxOut) == 0 {
		return RuleError("transaction has no outputs")
	}

	// NOTE: bitcoind does size limits checking here, but the size limits
	// have already been checked by btcwire for incoming transactions.
	// Also, btcwire checks the size limits on send too, so there is no need
	// to double check it here.

	// Ensure the transaction amounts are in range.  Each transaction
	// output must not be negative or more than the max allowed per
	// transaction.  Also, the total of all outputs must abide by the same
	// restrictions.  All amounts in a transaction are in a unit value known
	// as a satoshi.  One bitcoin is a quantity of satoshi as defined by the
	// satoshiPerBitcoin constant.
	var totalSatoshi int64
	for _, txOut := range tx.TxOut {
		satoshi := txOut.Value
		if satoshi < 0 {
			str := fmt.Sprintf("transaction output has negative "+
				"value of %v", satoshi)
			return RuleError(str)
		}
		if satoshi > maxSatoshi {
			str := fmt.Sprintf("transaction output value of %v is "+
				"higher than max allowed value of %v", satoshi,
				maxSatoshi)
			return RuleError(str)
		}

		// TODO(davec): No need to check < 0 here as satoshi is
		// guaranteed to be positive per the above check.  Also need
		// to add overflow checks.
		totalSatoshi += satoshi
		if totalSatoshi < 0 {
			str := fmt.Sprintf("total value of all transaction "+
				"outputs has negative value of %v", totalSatoshi)
			return RuleError(str)
		}
		if totalSatoshi > maxSatoshi {
			str := fmt.Sprintf("total value of all transaction "+
				"outputs is %v which is higher than max "+
				"allowed value of %v", totalSatoshi, maxSatoshi)
			return RuleError(str)
		}
	}

	// Check for duplicate transaction inputs.
	existingTxOut := make(map[string]bool)
	for _, txIn := range tx.TxIn {
		prevOut := &txIn.PreviousOutpoint
		key := fmt.Sprintf("%v%v", prevOut.Hash, prevOut.Index)
		if _, exists := existingTxOut[key]; exists {
			return RuleError("transaction contains duplicate outpoint")
		}
		existingTxOut[key] = true
	}

	// Coinbase script length must be between min and max length.
	if isCoinBase(tx) {
		slen := len(tx.TxIn[0].SignatureScript)
		if slen < minCoinbaseScriptLen || slen > maxCoinbaseScriptLen {
			str := fmt.Sprintf("coinbase transaction script length "+
				"of %d is out of range (min: %d, max: %d)",
				slen, minCoinbaseScriptLen, maxCoinbaseScriptLen)
			return RuleError(str)
		}
	} else {
		// Previous transaction outputs referenced by the inputs to this
		// transaction must not be null.
		for _, txIn := range tx.TxIn {
			prevOut := &txIn.PreviousOutpoint
			if isNullOutpoint(prevOut) {
				return RuleError("transaction input refers to " +
					"previous output that is null")
			}
		}
	}

	return nil
}

// checkProofOfWork ensures the block header bits which indicate the target
// difficulty is in min/max range and that the block hash is less than the
// target difficulty as claimed.
func (b *BlockChain) checkProofOfWork(block *btcutil.Block) error {
	// The target difficulty must be larger than zero.
	header := block.MsgBlock().Header
	target := CompactToBig(header.Bits)
	if target.Sign() <= 0 {
		str := fmt.Sprintf("block target difficulty of %064x is too low",
			target)
		return RuleError(str)
	}

	// The target difficulty must be less than the maximum allowed.
	powLimit := b.netParams().powLimit
	if target.Cmp(powLimit) > 0 {
		str := fmt.Sprintf("block target difficulty of %064x is "+
			"higher than max of %064x", target, powLimit)
		return RuleError(str)
	}

	// The block hash must be less than the claimed target.
	blockHash, err := block.Sha()
	if err != nil {
		return err
	}
	hashNum := ShaHashToBig(blockHash)
	if hashNum.Cmp(target) > 0 {
		str := fmt.Sprintf("block hash of %064x is higher than "+
			"expected max of %064x", hashNum, target)
		return RuleError(str)
	}

	return nil
}

// countSigOps returns the number of signature operations for all transaction
// input and output scripts in the provided transaction.  This uses the
// quicker, but imprecise, signature operation counting mechanism from
// btcscript.
func countSigOps(msgTx *btcwire.MsgTx) int {
	// Accumulate the number of signature operations in all transaction
	// inputs.
	totalSigOps := 0
	for _, txIn := range msgTx.TxIn {
		numSigOps := btcscript.GetSigOpCount(txIn.SignatureScript)
		totalSigOps += numSigOps
	}

	// Accumulate the number of signature operations in all transaction
	// outputs.
	for _, txOut := range msgTx.TxOut {
		numSigOps := btcscript.GetSigOpCount(txOut.PkScript)
		totalSigOps += numSigOps
	}

	return totalSigOps
}

// countP2SHSigOps returns the number of signature operations for all input
// transactions which are of the pay-to-script-hash type.  This uses the
// precise, signature operation counting mechanism from btcscript which requires
// access to the input transaction scripts.
func countP2SHSigOps(msgTx *btcwire.MsgTx, isCoinBaseTx bool, txStore map[btcwire.ShaHash]*txData) (int, error) {
	// Coinbase transactions have no interesting inputs.
	if isCoinBaseTx {
		return 0, nil
	}

	// TODO(davec): Need to pass the cached version in.
	txHash, err := msgTx.TxSha(btcwire.ProtocolVersion)
	if err != nil {
		return 0, err
	}

	// Accumulate the number of signature operations in all transaction
	// inputs.
	totalSigOps := 0
	for _, txIn := range msgTx.TxIn {
		// Ensure the referenced input transaction is available.
		txInHash := &txIn.PreviousOutpoint.Hash
		originTx, exists := txStore[*txInHash]
		if !exists || originTx.err != nil || originTx.tx == nil {
			return 0, fmt.Errorf("unable to find input transaction "+
				"%v referenced from transaction %v", txHash,
				txInHash)
		}

		// Ensure the output index in the referenced transaction is
		// available.
		originTxIndex := txIn.PreviousOutpoint.Index
		if originTxIndex >= uint32(len(originTx.tx.TxOut)) {
			return 0, fmt.Errorf("out of bounds input index %d in "+
				"transaction %v referenced from transaction %v",
				originTxIndex, txInHash, txHash)
		}

		// We're only interested in pay-to-script-hash types, so skip
		// this input if it's not one.
		pkScript := originTx.tx.TxOut[originTxIndex].PkScript
		if !btcscript.IsPayToScriptHash(pkScript) {
			continue
		}

		// Count the precise number of signature operations in the
		// referenced public key script.
		sigScript := txIn.SignatureScript
		numSigOps := btcscript.GetPreciseSigOpCount(sigScript, pkScript,
			true)

		// We could potentially overflow the accumulator so check for
		// overflow.
		lastSigOps := totalSigOps
		totalSigOps += numSigOps
		if totalSigOps < lastSigOps {
			return 0, fmt.Errorf("the public key script from "+
				"output index %d in transaction %v contains "+
				"too many signature operations - overflow",
				originTxIndex, txInHash)
		}
	}

	return totalSigOps, nil
}

// checkBlockSanity performs some preliminary checks on a block to ensure it is
// sane before continuing with block processing.  These checks are context free.
func (b *BlockChain) checkBlockSanity(block *btcutil.Block) error {
	// NOTE: bitcoind does size limits checking here, but the size limits
	// have already been checked by btcwire for incoming blocks.  Also,
	// btcwire checks the size limits on send too, so there is no need
	// to double check it here.

	// Ensure the proof of work bits in the block header is in min/max range
	// and the block hash is less than the target value described by the
	// bits.
	err := b.checkProofOfWork(block)
	if err != nil {
		return err
	}

	// Ensure the block time is not more than 2 hours in the future.
	msgBlock := block.MsgBlock()
	header := &msgBlock.Header
	if header.Timestamp.After(time.Now().Add(time.Hour * 2)) {
		str := fmt.Sprintf("block timestamp of %v is too far in the "+
			"future", header.Timestamp)
		return RuleError(str)
	}

	// A block must have at least one transaction.
	transactions := msgBlock.Transactions
	if len(transactions) == 0 {
		return RuleError("block does not contain any transactions")
	}

	// The first transaction in a block must be a coinbase.
	if !isCoinBase(transactions[0]) {
		return RuleError("first transaction in block is not a coinbase")
	}

	// A block must not have more than one coinbase.
	for i, tx := range transactions[1:] {
		if isCoinBase(tx) {
			str := fmt.Sprintf("block contains second coinbase at "+
				"index %d", i)
			return RuleError(str)
		}
	}

	// Do some preliminary checks on each transaction to ensure they are
	// sane before continuing.
	for _, tx := range transactions {
		err := checkTransactionSanity(tx)
		if err != nil {
			return err
		}
	}

	// Build merkle tree and ensure the calculated merkle root matches the
	// entry in the block header.  This also has the effect of caching all
	// of the transaction hashes in the block to speed up future hash
	// checks.  Bitcoind builds the tree here and checks the merkle root
	// after the following checks, but there is no reason not to check the
	// merkle root matches here.
	merkles := BuildMerkleTreeStore(block)
	calculatedMerkleRoot := merkles[len(merkles)-1]
	if !header.MerkleRoot.IsEqual(calculatedMerkleRoot) {
		str := fmt.Sprintf("block merkle root is invalid - got %v, "+
			"want %v", calculatedMerkleRoot, header.MerkleRoot)
		return RuleError(str)
	}

	// Check for duplicate transactions.  This check will be fairly quick
	// since the transaction hashes are already cached due to building the
	// merkle tree above.
	existingTxHashes := make(map[btcwire.ShaHash]bool)
	txShas, err := block.TxShas()
	if err != nil {
		return err
	}
	for _, hash := range txShas {
		if _, exists := existingTxHashes[*hash]; exists {
			str := fmt.Sprintf("block contains duplicate "+
				"transaction %v", hash)
			return RuleError(str)
		}
		existingTxHashes[*hash] = true
	}

	// The number of signature operations must be less than the maximum
	// allowed per block.
	totalSigOps := 0
	for _, tx := range transactions {
		// We could potentially overflow the accumulator so check for
		// overflow.
		lastSigOps := totalSigOps
		totalSigOps += countSigOps(tx)
		if totalSigOps < lastSigOps || totalSigOps > maxSigOpsPerBlock {
			str := fmt.Sprintf("block contains too many signature "+
				"operations - got %v, max %v", totalSigOps,
				maxSigOpsPerBlock)
			return RuleError(str)
		}
	}

	return nil
}

// checkSerializedHeight checks if the signature script in the passed
// transaction starts with the serialized block height of wantHeight.
func checkSerializedHeight(coinbaseTx *btcwire.MsgTx, wantHeight int64) error {
	sigScript := coinbaseTx.TxIn[0].SignatureScript
	if len(sigScript) < 4 {
		str := "the coinbase signature script for blocks of " +
			"version %d or greater must start with the " +
			"serialized block height"
		str = fmt.Sprintf(str, serializedHeightVersion)
		return RuleError(str)
	}

	serializedHeightBytes := make([]byte, 4, 4)
	copy(serializedHeightBytes, sigScript[1:4])
	serializedHeight := binary.LittleEndian.Uint32(serializedHeightBytes)
	if int64(serializedHeight) != wantHeight {
		str := fmt.Sprintf("the coinbase signature script serialized "+
			"block height is %d when %d was expected",
			serializedHeight, wantHeight)
		return RuleError(str)
	}

	return nil
}

// isTransactionSpent returns whether or not the provided transaction is fully
// spent.  A fully spent transaction is one where all outputs have been spent.
func isTransactionSpent(tx *txData) bool {
	for _, isOutputSpent := range tx.spent {
		if !isOutputSpent {
			return false
		}
	}
	return true
}

// checkBIP0030 ensures blocks do not contain duplicate transactions which
// 'overwrite' older transactions that are not fully spent.  This prevents an
// attack where a coinbase and all of its dependent transactions could be
// duplicated to effectively revert the overwritten transactions to a single
// confirmation thereby making them vulnerable to a double spend.
//
// For more details, see https://en.bitcoin.it/wiki/BIP_0030 and
// http://r6.ca/blog/20120206T005236Z.html.
func (b *BlockChain) checkBIP0030(node *blockNode, block *btcutil.Block) error {
	// Attempt to fetch duplicate transactions for all of the transactions
	// in this block from the point of view of the parent node.
	fetchList, err := block.TxShas()
	if err != nil {
		return nil
	}
	txResults, err := b.fetchTxList(node, fetchList)
	if err != nil {
		return err
	}

	// Examine the resulting data about the requested transactions.
	for _, txD := range txResults {
		switch txD.err {
		// A duplicate transaction was not found.  This is the most
		// common case.
		case btcdb.TxShaMissing:
			continue

		// A duplicate transaction was found.  This is only allowed if
		// the duplicate transaction is fully spent.
		case nil:
			if !isTransactionSpent(txD) {
				str := fmt.Sprintf("tried to overwrite "+
					"transaction %v at block height %d "+
					"that is not fully spent", txD.hash,
					txD.blockHeight)
				return RuleError(str)
			}

		// Some other unexpected error occurred.  Return it now.
		default:
			return txD.err
		}
	}

	return nil
}

// checkTransactionInputs performs a series of checks on the inputs to a
// transaction to ensure they are valid.  An example of some of the checks
// include verifying all inputs exist, ensuring the coinbase seasoning
// requirements are met, detecting double spends, validating all values and fees
// are in the legal range and the total output amount doesn't exceed the input
// amount, and verifying the signatures to prove the spender was the owner of
// the bitcoins and therefore allowed to spend them.  As it checks the inputs,
// it also calculates the total fees for the transaction and returns that value.
func checkTransactionInputs(tx *btcwire.MsgTx, txHeight int64, txStore map[btcwire.ShaHash]*txData) (int64, error) {
	// Coinbase transactions have no inputs.
	if isCoinBase(tx) {
		return 0, nil
	}

	// TODO(davec): Need to pass the cached version in.
	txHash, err := tx.TxSha(btcwire.ProtocolVersion)
	if err != nil {
		return 0, err
	}

	var totalSatoshiIn int64
	for _, txIn := range tx.TxIn {
		// Ensure the input is available.
		txInHash := &txIn.PreviousOutpoint.Hash
		originTx, exists := txStore[*txInHash]
		if !exists {
			str := fmt.Sprintf("unable to find input transaction "+
				"%v for transaction %v", txHash, txInHash)
			return 0, RuleError(str)
		}

		// Ensure the transaction is not spending coins which have not
		// yet reached the required coinbase maturity.
		if isCoinBase(originTx.tx) {
			originHeight := originTx.blockHeight
			blocksSincePrev := txHeight - originHeight
			if blocksSincePrev < coinbaseMaturity {
				str := fmt.Sprintf("tried to spend coinbase "+
					"transaction %v from height %v at "+
					"height %v before required maturity "+
					"of %v blocks", txHash, originHeight,
					txHeight, coinbaseMaturity)
				return 0, RuleError(str)
			}
		}

		// Ensure the transaction is not double spending coins.
		originTxIndex := txIn.PreviousOutpoint.Index
		if originTxIndex >= uint32(len(originTx.spent)) {
			return 0, fmt.Errorf("out of bounds input index %d in "+
				"transaction %v referenced from transaction %v",
				originTxIndex, txInHash, txHash)
		}
		if originTx.spent[originTxIndex] {
			str := fmt.Sprintf("transaction %v tried to double "+
				"spend coins from transaction %v", txHash,
				txInHash)
			return 0, RuleError(str)
		}

		// Ensure the transaction amounts are in range.  Each of the
		// output values of the input transactions must not be negative
		// or more than the max allowed per transaction.  All amounts in
		// a transaction are in a unit value known as a satoshi.  One
		// bitcoin is a quantity of satoshi as defined by the
		// satoshiPerBitcoin constant.
		originTxSatoshi := originTx.tx.TxOut[originTxIndex].Value
		if originTxSatoshi < 0 {
			str := fmt.Sprintf("transaction output has negative "+
				"value of %v", originTxSatoshi)
			return 0, RuleError(str)
		}
		if originTxSatoshi > maxSatoshi {
			str := fmt.Sprintf("transaction output value of %v is "+
				"higher than max allowed value of %v",
				originTxSatoshi, maxSatoshi)
			return 0, RuleError(str)
		}

		// The total of all outputs must not be more than the max
		// allowed per transaction.  Also, we could potentially overflow
		// the accumulator so check for overflow.
		lastSatoshiIn := totalSatoshiIn
		totalSatoshiIn += originTxSatoshi
		if totalSatoshiIn < lastSatoshiIn || totalSatoshiIn > maxSatoshi {
			str := fmt.Sprintf("total value of all transaction "+
				"inputs is %v which is higher than max "+
				"allowed value of %v", totalSatoshiIn,
				maxSatoshi)
			return 0, RuleError(str)
		}

		// Mark the referenced output as spent.
		originTx.spent[originTxIndex] = true
	}

	// Calculate the total output amount for this transaction.  It is safe
	// to ignore overflow and out of range errors here because those error
	// conditions would have already been caught by checkTransactionSanity.
	var totalSatoshiOut int64
	for _, txOut := range tx.TxOut {
		totalSatoshiOut += txOut.Value
	}

	// Ensure the transaction does not spend more than its inputs.
	if totalSatoshiIn < totalSatoshiOut {
		str := fmt.Sprintf("total value of all transaction inputs for "+
			"transaction %v is %v which is less than the amount "+
			"spent of %v", txHash, totalSatoshiIn, totalSatoshiOut)
		return 0, RuleError(str)
	}

	// NOTE: bitcoind checks if the transaction fees are < 0 here, but that
	// is an impossible condition because of the check above that ensures
	// the inputs are >= the outputs.
	txFeeInSatoshi := totalSatoshiIn - totalSatoshiOut
	return txFeeInSatoshi, nil
}

// checkConnectBlock performs several checks to confirm connecting the passed
// block to the main chain (including whatever reorganization might be necessary
// to get this node to the main chain) does not violate any rules.
func (b *BlockChain) checkConnectBlock(node *blockNode, block *btcutil.Block) error {
	// If the side chain blocks end up in the database, a call to
	// checkBlockSanity should be done here in case a previous version
	// allowed a block that is no longer valid.  However, since the
	// implementation only currently uses memory for the side chain blocks,
	// it isn't currently necessary.

	// TODO(davec): Keep a flag if this has already been done to avoid
	// multiple runs.

	// The coinbase for the Genesis block is not spendable, so just return
	// now.
	if node.hash.IsEqual(&btcwire.GenesisHash) {
		return nil
	}

	// BIP0030 added a rule to prevent blocks which contain duplicate
	// transactions that 'overwrite' older transactions which are not fully
	// spent.  See the documentation for checkBIP0030 for more details.
	//
	// There are two blocks in the chain which violate this
	// rule, so the check must be skipped for those blocks. The
	// isBIP0030Node function is used to determine if this block is one
	// of the two blocks that must be skipped.
	enforceBIP0030 := !isBIP0030Node(node)
	if enforceBIP0030 {
		err := b.checkBIP0030(node, block)
		if err != nil {
			return err
		}
	}

	// Request a map that contains all input transactions for the block from
	// the point of view of its position within the block chain.  These
	// transactions are needed for verification of things such as
	// transaction inputs, counting pay-to-script-hashes, and scripts.
	txInputStore, err := b.fetchInputTransactions(node, block)
	if err != nil {
		return err
	}

	// BIP0016 describes a pay-to-script-hash type that is considered a
	// "standard" type.  The rules for this BIP only apply to transactions
	// after the timestmap defined by btcscript.Bip16Activation. See
	// https://en.bitcoin.it/wiki/BIP_0016 for more details.
	enforceBIP0016 := false
	if node.timestamp.After(btcscript.Bip16Activation) {
		enforceBIP0016 = true
	}

	// The number of signature operations must be less than the maximum
	// allowed per block.  Note that the preliminary sanity checks on a
	// block also include a check similar to this one, but this check
	// expands the count to include a precise count of pay-to-script-hash
	// signature operations in each of the input transaction public key
	// scripts.
	transactions := block.MsgBlock().Transactions
	totalSigOps := 0
	for i, tx := range transactions {
		numsigOps := countSigOps(tx)
		if enforceBIP0016 {
			// Since the first (and only the first) transaction has
			// already been verified to be a coinbase transaction,
			// use i == 0 as an optimization for the flag to
			// countP2SHSigOps for whether or not the transaction is
			// a coinbase transaction rather than having to do a
			// full coinbase check again.
			numP2SHSigOps, err := countP2SHSigOps(tx, i == 0,
				txInputStore)
			if err != nil {
				return err
			}
			numsigOps += numP2SHSigOps
		}

		// Check for overflow or going over the limits.  We have to do
		// this on every loop to avoid overflow.
		lastSigops := totalSigOps
		totalSigOps += numsigOps
		if totalSigOps < lastSigops || totalSigOps > maxSigOpsPerBlock {
			str := fmt.Sprintf("block contains too many "+
				"signature operations - got %v, max %v",
				totalSigOps, maxSigOpsPerBlock)
			return RuleError(str)
		}
	}

	// Perform several checks on the inputs for each transaction.  Also
	// accumulate the total fees.  This could technically be combined with
	// the loop above instead of running another loop over the transactions,
	// but by separating it we can avoid running the more expensive (though
	// still relatively cheap as compared to running the scripts) checks
	// against all the inputs when the signature operations are out of
	// bounds.
	var totalFees int64
	for _, tx := range transactions {
		txFee, err := checkTransactionInputs(tx, node.height, txInputStore)
		if err != nil {
			return err
		}

		// Sum the total fees and ensure we don't overflow the
		// accumulator.
		lastTotalFees := totalFees
		totalFees += txFee
		if totalFees < lastTotalFees {
			return RuleError("total fees for block overflows " +
				"accumulator")
		}
	}

	// The total output values of the coinbase transaction must not exceed
	// the expected subsidy value plus total transaction fees gained from
	// mining the block.  It is safe to ignore overflow and out of range
	// errors here because those error conditions would have already been
	// caught by checkTransactionSanity.
	var totalSatoshiOut int64
	for _, txOut := range transactions[0].TxOut {
		totalSatoshiOut += txOut.Value
	}
	expectedSatoshiOut := calcBlockSubsidy(node.height) + totalFees
	if totalSatoshiOut > expectedSatoshiOut {
		str := fmt.Sprintf("coinbase transaction for block pays %v "+
			"which is more than expected value of %v",
			totalSatoshiOut, expectedSatoshiOut)
		return RuleError(str)
	}

	// Don't run scripts if this node is before the latest known good
	// checkpoint since the validity is verified via the checkpoints (all
	// transactions are included in the merkle root hash and any changes
	// will therefore be detected by the next checkpoint).  This is a huge
	// optimization because running the scripts is the most time consuming
	// portion of block handling.
	checkpoint := b.LatestCheckpoint()
	runScripts := !b.noVerify
	if checkpoint != nil && node.height <= checkpoint.Height {
		runScripts = false
	}

	// Now that the inexpensive checks are done and have passed, verify the
	// transactions are actually allowed to spend the coins by running the
	// expensive ECDSA signature check scripts.  Doing this last helps
	// prevent CPU exhaustion attacks.
	if runScripts {
		err := checkBlockScripts(block, txInputStore)
		if err != nil {
			return err
		}
	}

	return nil
}
