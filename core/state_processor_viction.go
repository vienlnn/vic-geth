// Copyright 2026 The Vic-geth Authors
package core

import (
	"fmt"
	"math/big"
	"runtime"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/misc"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vrc25"
	"github.com/ethereum/go-ethereum/legacy/tomox/tradingstate"
	"github.com/ethereum/go-ethereum/legacy/tomoxlending/lendingstate"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
)

// TradingEngine is the interface the TomoX engine must satisfy.
// Defined here to avoid an import cycle between core and legacy/tomox.
type TradingEngine interface {
	// CommitOrder replays a single order through the matching engine,
	// mutating statedb and tradingStateDB.
	CommitOrder(
		header *types.Header,
		coinbase common.Address,
		chain tradingstate.ChainContext,
		statedb *state.StateDB,
		tradingStateDB *tradingstate.TradingStateDB,
		orderBook common.Hash,
		order *tradingstate.OrderItem,
	) ([]map[string]string, []*tradingstate.OrderItem, error)

	// GetTradingState opens the TradingStateDB trie rooted at the given block.
	GetTradingState(block *types.Block, author common.Address) (*tradingstate.TradingStateDB, error)

	// GetTradingStateRoot returns the trading state root committed in the 0x92 tx
	// of the given block.
	GetTradingStateRoot(block *types.Block, author common.Address) (common.Hash, error)

	// UpdateMediumPriceBeforeEpoch computes epoch-averaged prices; must be called
	// before order matching at epoch boundaries.
	UpdateMediumPriceBeforeEpoch(epoch uint64, tradingStateDB *tradingstate.TradingStateDB, statedb *state.StateDB) error

	// GetStateCache returns the trie-node cache backed by the tomox LevelDB.
	// Used to flush trie nodes to disk after each block.
	GetStateCache() tradingstate.Database
}

// victionProcessorState holds per-block Viction state that is reset each block.
type victionProcessorState struct {
	currentBlockNumber *big.Int

	// tradingStateDB is the TomoX order-book trie. Non-nil only for pre-Atlas blocks.
	tradingStateDB *tradingstate.TradingStateDB

	// lendingStateDB is the TomoZ lending order-book trie. Non-nil only for pre-Atlas blocks.
	lendingStateDB *lendingstate.LendingStateDB

	// committedTradingRoot is the root returned by tradingStateDB.Commit() in afterProcess.
	// It is already staged into the trie.Database dirty set and ready to flush to LevelDB.
	committedTradingRoot common.Hash

	// committedLendingRoot is the root returned by lendingStateDB.Commit() in afterProcess.
	committedLendingRoot common.Hash

	// Pre-Atlas VRC25 fee tracking: running balances per token, updated each tx,
	// flushed to state in afterProcess.
	feeBalance map[common.Address]*big.Int // token -> remaining capacity
	feeUpdated map[common.Address]*big.Int // tokens with fees charged -> final capacity
	totalFee   *big.Int                    // total fees charged this block
}

func (p *StateProcessor) beforeProcess(block *types.Block, statedb *state.StateDB) error {
	header := block.Header()

	p.victionState = &victionProcessorState{
		currentBlockNumber: new(big.Int).Set(header.Number),
		feeUpdated:         map[common.Address]*big.Int{},
		totalFee:           new(big.Int),
	}

	if p.config.TIPSigningBlock != nil && p.config.TIPSigningBlock.Cmp(header.Number) == 0 {
		if p.config.Viction != nil {
			statedb.DeleteAddress(p.config.Viction.ValidatorBlockSignContract)
		}
	}
	if p.config.Viction != nil {
		if p.config.IsAtlas(header.Number) {
			misc.ApplyVIPVRC25Upgrade(statedb, p.config.Viction, p.config.AtlasBlock, header.Number)
		}
		if p.config.IsSaigon(block.Number()) {
			misc.ApplySaigonHardFork(statedb, p.config.Viction, p.config.SaigonBlock, block.Number())
		}
	}

	// Pre-Atlas: snapshot all VRC25 token fee capacities for per-tx eligibility checks.
	if !p.config.IsAtlas(header.Number) &&
		p.config.Viction != nil && p.config.Viction.VRC25Contract != (common.Address{}) {
		p.victionState.feeBalance = vrc25.GetAllFeeCapacities(statedb, p.config.Viction.VRC25Contract)
	}

	// Open the TomoX trading trie from the parent block.
	if p.tradingEngine != nil && p.config.Posv != nil &&
		p.config.IsTomoXEnabled(header.Number) && header.Number.Uint64() > p.config.Posv.Epoch {

		parent := p.bc.GetBlock(header.ParentHash, header.Number.Uint64()-1)
		if parent != nil {
			parentAuthor, _ := p.engine.Author(parent.Header())
			tradingState, err := p.tradingEngine.GetTradingState(parent, parentAuthor)
			if err != nil {
				return fmt.Errorf("TomoX: failed to open TradingStateDB at block %d: %w", header.Number, err)
			}
			p.victionState.tradingStateDB = tradingState

			if header.Number.Uint64()%p.config.Posv.Epoch == 0 {
				if err := p.tradingEngine.UpdateMediumPriceBeforeEpoch(
					header.Number.Uint64()/p.config.Posv.Epoch,
					tradingState, statedb,
				); err != nil {
					return fmt.Errorf("TomoX: UpdateMediumPriceBeforeEpoch failed at block %d: %w", header.Number, err)
				}
			}
		}
	}

	// Open the TomoZ lending trie; requires tradingStateDB to be ready.
	if p.lendingEngine != nil && p.config.Posv != nil &&
		p.config.IsTomoXLendingEnabled(header.Number) && header.Number.Uint64() > p.config.Posv.Epoch &&
		p.victionState.tradingStateDB != nil {

		parent := p.bc.GetBlock(header.ParentHash, header.Number.Uint64()-1)
		if parent != nil {
			parentAuthor, _ := p.engine.Author(parent.Header())
			lendingState, err := p.lendingEngine.GetLendingState(parent, parentAuthor)
			if err != nil {
				return fmt.Errorf("TomoZ: failed to open LendingStateDB at block %d: %w", header.Number, err)
			}
			p.victionState.lendingStateDB = lendingState
		}
	}

	InitSignerInTransactions(p.config, header, block.Transactions())

	return nil
}

func (p *StateProcessor) afterProcess(block *types.Block, statedb *state.StateDB) error {
	// Pre-Atlas: flush accumulated VRC25 fee updates to state.
	if p.victionState != nil && !p.config.IsAtlas(block.Number()) &&
		p.config.Viction != nil && p.config.Viction.VRC25Contract != (common.Address{}) &&
		len(p.victionState.feeUpdated) > 0 {
		vrc25.UpdateFeeCapacity(statedb, p.config.Viction.VRC25Contract, p.victionState.feeUpdated, p.victionState.totalFee)
	}

	// Commit and verify the TomoX trading state root.
	//
	// IMPORTANT: tradingStateDB.Commit() must be called BEFORE IntermediateRoot().
	// IntermediateRoot calls trie.Hash() which collapses in-memory dirty nodes into
	// hash nodes.  A subsequent trie.Commit() on a fully-hashed trie finds no dirty
	// nodes and writes nothing to trie.Database.dirties.  If Commit() runs first, it
	// flushes dirty nodes into trie.Database.dirties; trie.Database.Commit() can then
	// persist them to LevelDB.
	if p.victionState != nil && p.victionState.tradingStateDB != nil && p.tradingEngine != nil {
		tradingRoot, err := p.victionState.tradingStateDB.Commit()
		if err != nil {
			return fmt.Errorf("TomoX: TradingStateDB.Commit failed at block %d: %w", block.NumberU64(), err)
		}
		p.victionState.committedTradingRoot = tradingRoot

		blockAuthor, err := p.engine.Author(block.Header())
		if err != nil {
			return fmt.Errorf("TomoX: failed to resolve block author at block %d: %w", block.NumberU64(), err)
		}
		expectRoot := GetTradingStateRoot(block, p.config.Viction.TradingStateContract, blockAuthor, p.config)
		if tradingRoot != expectRoot {
			return fmt.Errorf("TomoX: trading state root mismatch at block %d: got %s, expected %s",
				block.NumberU64(), tradingRoot.Hex(), expectRoot.Hex())
		}
		log.Debug("TomoX: trading state root verified", "block", block.NumberU64(), "root", tradingRoot.Hex())
	}

	// Commit and verify the TomoZ lending state root (same logic as above).
	if p.victionState != nil && p.victionState.lendingStateDB != nil && p.victionState.tradingStateDB != nil {
		lendingRoot, err := p.victionState.lendingStateDB.Commit()
		if err != nil {
			return fmt.Errorf("TomoZ: LendingStateDB.Commit failed at block %d: %w", block.NumberU64(), err)
		}
		p.victionState.committedLendingRoot = lendingRoot

		blockAuthor, err := p.engine.Author(block.Header())
		if err != nil {
			return fmt.Errorf("TomoZ: failed to resolve block author at block %d: %w", block.NumberU64(), err)
		}
		expectRoot := GetLendingStateRoot(block, p.config.Viction.TradingStateContract, blockAuthor, p.config)
		if lendingRoot != expectRoot {
			return fmt.Errorf("TomoZ: lending state root mismatch at block %d: got %s, expected %s",
				block.NumberU64(), lendingRoot.Hex(), expectRoot.Hex())
		}
		log.Debug("TomoZ: lending state root verified", "block", block.NumberU64(), "root", lendingRoot.Hex())
	}

	return nil
}

func (p *StateProcessor) beforeApplyTransaction(block *types.Block, tx *types.Transaction, msg types.Message, statedb *state.StateDB) error {
	if p.config.Viction == nil {
		return nil
	}
	header := block.Header()

	if header.Number.BitLen() <= 64 && header.Number.Uint64() <= 9147459 {
		if val := p.config.Viction.GetVictionBypassBalance(header.Number.Uint64(), msg.From()); val != nil {
			statedb.SetBalance(msg.From(), val)
		}
	}

	if p.config.IsTIPBlacklist(block.Number()) {
		if p.config.Viction.IsBlacklisted(msg.From()) {
			return ErrBlacklistedAddress
		}
		if tx.To() != nil && p.config.Viction.IsBlacklisted(*tx.To()) {
			return ErrBlacklistedAddress
		}
	}

	return nil
}

func (p *StateProcessor) applyVictionTransaction(statedb *state.StateDB, tx *types.Transaction, header *types.Header, usedGas *uint64) (bool, *types.Receipt, uint64, error, *big.Int) {
	if tx.To() == nil || p.config.Viction == nil {
		return false, nil, 0, nil, nil
	}
	vicConfig := p.config.Viction

	// 0x89 — block-signer transaction
	if tx.IsSigningTransaction(vicConfig.ValidatorBlockSignContract) && p.config.IsTIPSigning(header.Number) {
		return p.applySignTransaction(statedb, tx, header, usedGas)
	}

	// 0x91 — TomoX order-matching batch (active only in TIPTomoX..Atlas window).
	if tx.IsTradingTransaction(vicConfig.TomoXContract) && p.config.IsTomoXEnabled(header.Number) {
		if _, err := tradingstate.DecodeTxMatchesBatch(tx.Data()); err == nil {
			return p.applyTomoXTx(statedb, tx, header, usedGas)
		}
		// Decode failed — let the normal EVM path handle it.
	}

	// 0x92 — trading state root commit; verified in afterProcess
	if tx.To() != nil && *tx.To() == vicConfig.TradingStateContract && p.config.IsTomoXEnabled(header.Number) {
		return p.applyEmptyTransaction(statedb, tx, header, usedGas)
	}

	// 0x93 — TomoZ lending order-matching batch.
	// Outer gate matches victionchain: IsTomoXEnabled (TIPTomoX..Atlas).
	// Inner matching only runs once TIPTomoXLending is also active.
	if tx.IsLendingTransaction(vicConfig.LendingContract) && p.config.IsTomoXEnabled(header.Number) {
		if _, err := lendingstate.DecodeTxLendingBatch(tx.Data()); err == nil {
			if p.config.IsTomoXLendingEnabled(header.Number) {
				return p.applyLendingTx(statedb, tx, header, usedGas)
			}
			return p.applyEmptyTransaction(statedb, tx, header, usedGas)
		}
		// Decode failed — let the normal EVM path handle it.
	}

	// 0x94 — lending finalized trade.
	// Same outer gate as victionchain: IsTomoXEnabled.
	if tx.IsLendingFinalizedTradeTransaction(vicConfig.LendingFinalizedContract) && p.config.IsTomoXEnabled(header.Number) {
		return p.applyEmptyTransaction(statedb, tx, header, usedGas)
	}

	return false, nil, 0, nil, nil
}

// applyEmptyTransaction produces a zero-gas receipt without running the EVM.
// Used for system transactions whose effects are handled outside the EVM (0x91–0x94).
func (p *StateProcessor) applyEmptyTransaction(statedb *state.StateDB, tx *types.Transaction, header *types.Header, usedGas *uint64) (bool, *types.Receipt, uint64, error, *big.Int) {
	var root []byte
	if p.config.IsByzantium(header.Number) {
		statedb.Finalise(true)
	} else {
		root = statedb.IntermediateRoot(p.config.IsEIP158(header.Number)).Bytes()
	}
	receipt := types.NewReceipt(root, false, *usedGas)
	receipt.TxHash = tx.Hash()
	receipt.GasUsed = 0

	log := &types.Log{}
	log.Address = *tx.To()
	log.BlockNumber = header.Number.Uint64()
	statedb.AddLog(log)
	receipt.Logs = statedb.GetLogs(tx.Hash())
	receipt.Bloom = types.CreateBloom(types.Receipts{receipt})

	return true, receipt, 0, nil, nil
}

func (p *StateProcessor) applySignTransaction(statedb *state.StateDB, tx *types.Transaction, header *types.Header, usedGas *uint64) (bool, *types.Receipt, uint64, error, *big.Int) {
	var root []byte
	if p.config.IsByzantium(header.Number) {
		statedb.Finalise(true)
	} else {
		root = statedb.IntermediateRoot(p.config.IsEIP158(header.Number)).Bytes()
	}
	from, err := types.Sender(types.MakeSigner(p.config, header.Number), tx)
	if err != nil {
		return true, nil, 0, err, nil
	}
	nonce := statedb.GetNonce(from)
	if nonce < tx.Nonce() {
		return true, nil, 0, ErrNonceTooHigh, nil
	} else if nonce > tx.Nonce() {
		return true, nil, 0, ErrNonceTooLow, nil
	}
	statedb.SetNonce(from, nonce+1)

	receipt := types.NewReceipt(root, false, *usedGas)
	receipt.TxHash = tx.Hash()
	receipt.GasUsed = 0

	log := &types.Log{}
	log.Address = p.config.Viction.ValidatorBlockSignContract
	log.BlockNumber = header.Number.Uint64()
	statedb.AddLog(log)
	receipt.Logs = statedb.GetLogs(tx.Hash())
	receipt.Bloom = types.CreateBloom(types.Receipts{receipt})

	return true, receipt, 0, nil, nil
}

func (p *StateProcessor) afterApplyTransaction(tx *types.Transaction, msg types.Message, statedb *state.StateDB, receipt *types.Receipt, usedGas uint64, err error) error {
	if p.victionState == nil || p.victionState.currentBlockNumber == nil {
		return nil
	}

	blockNum := p.victionState.currentBlockNumber

	if p.config.IsAtlas(blockNum) || tx.To() == nil {
		return nil
	}

	token := *tx.To()
	vicCfg := p.config.Viction

	// Pre-Atlas: accumulate VRC25 fee deductions into feeUpdated; flushed in afterProcess.
	if p.victionState.feeBalance != nil {
		runningCap, ok := p.victionState.feeBalance[token]
		if ok && runningCap != nil {
			fee := new(big.Int).SetUint64(usedGas)
			if p.config.TIPTRC21FeeBlock != nil && blockNum.Cmp(p.config.TIPTRC21FeeBlock) > 0 && vicCfg != nil && vicCfg.VRC25GasPrice != nil {
				fee = new(big.Int).Mul(fee, (*big.Int)(vicCfg.VRC25GasPrice))
			}
			if runningCap.Cmp(fee) > 0 {
				newCap := new(big.Int).Sub(runningCap, fee)
				p.victionState.feeBalance[token] = newCap
				p.victionState.feeUpdated[token] = newCap
				p.victionState.totalFee.Add(p.victionState.totalFee, fee)

				if receipt.Status == types.ReceiptStatusFailed {
					vrc25.PayFeeWithVRC25(statedb, msg.From(), token)
				}
			}
		}
	}

	return nil
}

// InitSignerInTransactions pre-caches sender addresses for all transactions in
// parallel, amortizing ECDSA recovery before the sequential transaction loop.
func InitSignerInTransactions(config *params.ChainConfig, header *types.Header, txs types.Transactions) {
	if txs.Len() == 0 {
		return
	}
	nWorker := runtime.NumCPU()
	signer := types.MakeSigner(config, header.Number)
	chunkSize := txs.Len() / nWorker
	if txs.Len()%nWorker != 0 {
		chunkSize++
	}
	wg := sync.WaitGroup{}
	wg.Add(nWorker)
	for i := 0; i < nWorker; i++ {
		from := i * chunkSize
		to := from + chunkSize
		if to > txs.Len() {
			to = txs.Len()
		}
		go func(from int, to int) {
			defer wg.Done()
			for j := from; j < to; j++ {
				types.CacheSigner(signer, txs[j])
				txs[j].CacheHash()
			}
		}(from, to)
	}
	wg.Wait()
}

// SetTradingEngine injects the TomoX trading engine into the state processor.
func (p *StateProcessor) SetTradingEngine(engine TradingEngine) {
	p.tradingEngine = engine
}

// SetLendingEngine injects the TomoZ lending engine into the state processor.
func (p *StateProcessor) SetLendingEngine(engine LendingEngine) {
	p.lendingEngine = engine
}

// applyTomoXTx decodes and replays a TomoX order-matching batch (0x91 transaction).
//
// Called only when the tx payload successfully decoded as a TxMatchBatch (guaranteed
// by the caller in applyVictionTransaction).
//
// On epoch-boundary blocks (block % Epoch == 0) skips order execution
// entirely and only runs UpdateMediumPriceBeforeEpoch (called in beforeProcess).
func (p *StateProcessor) applyTomoXTx(statedb *state.StateDB, tx *types.Transaction, header *types.Header, usedGas *uint64) (bool, *types.Receipt, uint64, error, *big.Int) {
	var root []byte
	if p.config.IsByzantium(header.Number) {
		statedb.Finalise(true)
	} else {
		root = statedb.IntermediateRoot(p.config.IsEIP158(header.Number)).Bytes()
	}

	isEpochBlock := p.config.Posv != nil && header.Number.Uint64()%p.config.Posv.Epoch == 0

	if !isEpochBlock && len(tx.Data()) > 0 && p.victionState != nil && p.victionState.tradingStateDB != nil && p.tradingEngine != nil {
		txMatchBatch, err := tradingstate.DecodeTxMatchesBatch(tx.Data())
		if err != nil {
			// Should not happen — caller pre-screened. Log and fall through to empty receipt.
			log.Warn("TomoX: unexpected decode failure in applyTomoXTx", "tx", tx.Hash(), "err", err)
		} else {
			coinbase := header.Coinbase
			tradingEngine := p.tradingEngine
			tradingStateDB := p.victionState.tradingStateDB

			for i, txDataMatch := range txMatchBatch.Data {
				order, err := txDataMatch.DecodeOrder()
				if err != nil {
					log.Warn("TomoX: failed to decode order, skipping", "index", i, "err", err)
					continue
				}

				orderBook := tradingstate.GetTradingOrderBookHash(order.BaseToken, order.QuoteToken)

				_, rejects, err := tradingEngine.CommitOrder(header, coinbase, p.bc, statedb, tradingStateDB, orderBook, order)
				if err != nil {
					return true, nil, 0, fmt.Errorf("TomoX: CommitOrder failed index=%d order=%s: %w", i, order.Hash.Hex(), err), nil
				}

				if len(rejects) > 0 {
					log.Debug("TomoX: orders rejected", "count", len(rejects))
				}
			}
		}
	}

	receipt := types.NewReceipt(root, false, *usedGas)
	receipt.TxHash = tx.Hash()
	receipt.GasUsed = 0

	txLog := &types.Log{}
	txLog.Address = *tx.To()
	txLog.BlockNumber = header.Number.Uint64()
	statedb.AddLog(txLog)
	receipt.Logs = statedb.GetLogs(tx.Hash())
	receipt.Bloom = types.CreateBloom(types.Receipts{receipt})

	return true, receipt, 0, nil, nil
}

// applyLendingTx decodes and replays a TomoZ lending order-matching batch (0x93 transaction).
//
// Called only when the tx payload successfully decoded as a TxLendingBatch (guaranteed
// by the caller in applyVictionTransaction).
//
// Mirrors the epoch-skip rule from applyTomoXTx: on epoch-boundary blocks
// skips all order execution; only UpdateMediumPriceBeforeEpoch runs that block.
func (p *StateProcessor) applyLendingTx(statedb *state.StateDB, tx *types.Transaction, header *types.Header, usedGas *uint64) (bool, *types.Receipt, uint64, error, *big.Int) {
	var root []byte
	if p.config.IsByzantium(header.Number) {
		statedb.Finalise(true)
	} else {
		root = statedb.IntermediateRoot(p.config.IsEIP158(header.Number)).Bytes()
	}

	isEpochBlock := p.config.Posv != nil && header.Number.Uint64()%p.config.Posv.Epoch == 0

	if !isEpochBlock &&
		len(tx.Data()) > 0 &&
		p.victionState != nil &&
		p.victionState.lendingStateDB != nil &&
		p.victionState.tradingStateDB != nil &&
		p.lendingEngine != nil {

		txMatchBatch, err := lendingstate.DecodeTxLendingBatch(tx.Data())
		if err != nil {
			// Should not happen — caller pre-screened. Log and fall through to empty receipt.
			log.Warn("TomoZ: unexpected decode failure in applyLendingTx", "tx", tx.Hash(), "err", err)
		} else {
			coinbase := header.Coinbase
			lendingStateDB := p.victionState.lendingStateDB
			tradingStateDB := p.victionState.tradingStateDB

			for i, order := range txMatchBatch.Data {
				if order == nil {
					continue
				}
				lendingOrderBook := lendingstate.GetLendingOrderBookHash(order.LendingToken, order.Term)
				_, rejects, err := p.lendingEngine.CommitOrder(
					header, coinbase, p.bc, statedb,
					lendingStateDB, tradingStateDB, lendingOrderBook, order,
				)
				if err != nil {
					return true, nil, 0, fmt.Errorf("TomoZ: CommitOrder failed index=%d: %w", i, err), nil
				}
				if len(rejects) > 0 {
					log.Debug("TomoZ: lending orders rejected", "count", len(rejects))
				}
			}
		}
	}

	receipt := types.NewReceipt(root, false, *usedGas)
	receipt.TxHash = tx.Hash()
	receipt.GasUsed = 0
	txLog := &types.Log{}
	txLog.Address = *tx.To()
	txLog.BlockNumber = header.Number.Uint64()
	statedb.AddLog(txLog)
	receipt.Logs = statedb.GetLogs(tx.Hash())
	receipt.Bloom = types.CreateBloom(types.Receipts{receipt})
	return true, receipt, 0, nil, nil
}

// GetLendingStateRoot extracts the TomoZ lending state root from the 0x92 tx.
// The tx payload is [32 bytes trading root | 32 bytes lending root]; this returns bytes [32:64].
func GetLendingStateRoot(block *types.Block, tradingStateAddr common.Address, author common.Address, config *params.ChainConfig) common.Hash {
	signer := types.MakeSigner(config, block.Number())
	for _, tx := range block.Transactions() {
		if tx.To() == nil || *tx.To() != tradingStateAddr {
			continue
		}
		from, err := types.Sender(signer, tx)
		if err != nil || from != author {
			continue
		}
		if len(tx.Data()) >= 64 {
			return common.BytesToHash(tx.Data()[32:])
		}
	}
	return lendingstate.EmptyRoot
}

// GetTradingStateRoot extracts the TomoX trading state root from the 0x92 tx.
// The tx payload is [32 bytes trading root | 32 bytes lending root]; this returns bytes [0:32].
func GetTradingStateRoot(block *types.Block, tradingStateAddr common.Address, author common.Address, config *params.ChainConfig) common.Hash {
	signer := types.MakeSigner(config, block.Number())
	for _, tx := range block.Transactions() {
		if tx.To() == nil || *tx.To() != tradingStateAddr {
			continue
		}
		from, err := types.Sender(signer, tx)
		if err != nil || from != author {
			continue
		}
		if len(tx.Data()) >= 32 {
			return common.BytesToHash(tx.Data()[:32])
		}
	}
	return tradingstate.EmptyRoot
}
