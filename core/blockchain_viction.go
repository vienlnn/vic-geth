// Copyright 2026 The Vic-geth Authors
package core

import (
	"fmt"
	"sort"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/posv"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/sortlgc"
)

// commitVictionState manages persistence of TomoX and TomoZ trie nodes using the
// same deferred GC strategy as the main EVM state trie (Reference + priority
// queue; Commit to LevelDB every TriesInMemory blocks; Dereference old roots).
//
// afterProcess calls TradingStateDB.Commit() / LendingStateDB.Commit() which
// stage dirty trie nodes into the respective trie.Database dirty sets.
//
// Here we:
//  1. Reference the new root to keep it in memory.
//  2. Push it onto the deferred GC queue (tradingTriegc / lendingTriegc).
//  3. Every TriesInMemory blocks: commit the root that is TriesInMemory behind
//     HEAD to LevelDB, then Dereference older roots that are no longer needed.
//
// This matches victionchain's WriteBlockWithState deferred commit strategy and
// avoids a LevelDB write on every single block (which would cause excessive
// write amplification).
func (bc *BlockChain) commitVictionState(block *types.Block) error {
	if !bc.chainConfig.IsTomoXEnabled(block.Number()) {
		return nil
	}
	if bc.cacheConfig.TrieDirtyDisabled {
		return bc.commitVictionStateDirect(block)
	}
	return bc.commitVictionStateDeferred(block)
}

// commitVictionStateDirect is used in archive mode (TrieDirtyDisabled): flush
// every block immediately to LevelDB, no deferred GC needed.
func (bc *BlockChain) commitVictionStateDirect(block *types.Block) error {
	sp, ok := bc.processor.(*StateProcessor)
	if !ok || sp.victionState == nil {
		return nil
	}
	if sp.victionState.tradingStateDB != nil && sp.tradingEngine != nil {
		tradingRoot := sp.victionState.committedTradingRoot
		if tradingRoot != (common.Hash{}) {
			if err := sp.tradingEngine.GetStateCache().TrieDB().Commit(tradingRoot, false, nil); err != nil {
				return fmt.Errorf("TomoX: trading trieDB.Commit(archive) failed at block %d: %w", block.NumberU64(), err)
			}
			log.Trace("TomoX: trading trie flushed to disk (archive)", "block", block.NumberU64(), "root", tradingRoot.Hex())
		}
	}
	if sp.victionState.lendingStateDB != nil && sp.lendingEngine != nil {
		lendingRoot := sp.victionState.committedLendingRoot
		if lendingRoot != (common.Hash{}) {
			if err := sp.lendingEngine.GetStateCache().TrieDB().Commit(lendingRoot, false, nil); err != nil {
				return fmt.Errorf("TomoZ: lending trieDB.Commit(archive) failed at block %d: %w", block.NumberU64(), err)
			}
			log.Trace("TomoZ: lending trie flushed to disk (archive)", "block", block.NumberU64(), "root", lendingRoot.Hex())
		}
	}
	return nil
}

// commitVictionStateDeferred is the full-node path for trading/lending trie persistence.
//
// Unlike the EVM trie, we commit the current block's root to LevelDB on every
// block rather than deferring the commit TriesInMemory blocks. This is
// necessary because GetTradingStateRoot (used in the victionchain deferred
// path) returns EmptyRoot for any block that has no 0x92 system tx, causing
// dirty nodes to accumulate in the trie.Database dirty cache until Dereference
// removes them without ever being written — the "nodes=0" bug.
//
// We still push every root onto the GC queue and defer the Dereference by
// TriesInMemory blocks, which is sufficient for reorg safety (same as EVM).
// The extra write overhead per block is small compared to order matching.
func (bc *BlockChain) commitVictionStateDeferred(block *types.Block) error {
	sp, ok := bc.processor.(*StateProcessor)
	if !ok || sp.victionState == nil {
		return nil
	}
	current := block.NumberU64()

	// Trading trie: commit the current block's dirty root immediately.
	if sp.victionState.tradingStateDB != nil && sp.tradingEngine != nil {
		tradingRoot := sp.victionState.committedTradingRoot
		if tradingRoot != (common.Hash{}) {
			tradingTrieDB := sp.tradingEngine.GetStateCache().TrieDB()
			tradingTrieDB.Reference(tradingRoot, common.Hash{})
			sp.tradingTriegc.Push(tradingRoot, -int64(current))

			if err := tradingTrieDB.Commit(tradingRoot, true, nil); err != nil {
				log.Error("TomoX: trading trieDB.Commit failed", "block", current, "err", err)
			} else {
				log.Trace("TomoX: trading trie flushed to disk", "block", current, "root", tradingRoot)
			}

			// Dereference roots old enough to no longer need keeping in memory.
			if current > TriesInMemory {
				chosen := current - TriesInMemory
				for !sp.tradingTriegc.Empty() {
					root, number := sp.tradingTriegc.Pop()
					if uint64(-number) > chosen {
						sp.tradingTriegc.Push(root, number)
						break
					}
					tradingTrieDB.Dereference(root.(common.Hash))
				}
			}
		}
	}

	// Lending trie: same strategy as trading trie.
	if sp.victionState.lendingStateDB != nil && sp.lendingEngine != nil {
		lendingRoot := sp.victionState.committedLendingRoot
		if lendingRoot != (common.Hash{}) {
			lendingTrieDB := sp.lendingEngine.GetStateCache().TrieDB()
			lendingTrieDB.Reference(lendingRoot, common.Hash{})
			sp.lendingTriegc.Push(lendingRoot, -int64(current))

			if err := lendingTrieDB.Commit(lendingRoot, true, nil); err != nil {
				log.Error("TomoZ: lending trieDB.Commit failed", "block", current, "err", err)
			} else {
				log.Trace("TomoZ: lending trie flushed to disk", "block", current, "root", lendingRoot)
			}

			if current > TriesInMemory {
				chosen := current - TriesInMemory
				for !sp.lendingTriegc.Empty() {
					root, number := sp.lendingTriegc.Pop()
					if uint64(-number) > chosen {
						sp.lendingTriegc.Push(root, number)
						break
					}
					lendingTrieDB.Dereference(root.(common.Hash))
				}
			}
		}
	}

	return nil
}

// stopViction flushes any in-memory trading/lending trie roots that were not yet
// committed to LevelDB (the tail of the deferred GC queues).  Called from
// BlockChain.Stop() before the node exits.
func (bc *BlockChain) stopViction() {
	if bc.cacheConfig.TrieDirtyDisabled {
		return // archive mode commits every block; nothing to flush here
	}
	sp, ok := bc.processor.(*StateProcessor)
	if !ok || sp == nil {
		return
	}

	// Flush all remaining trading trie roots to LevelDB.
	if sp.tradingEngine != nil {
		tradingTrieDB := sp.tradingEngine.GetStateCache().TrieDB()
		for !sp.tradingTriegc.Empty() {
			root := sp.tradingTriegc.PopItem().(common.Hash)
			if err := tradingTrieDB.Commit(root, true, nil); err != nil {
				log.Error("TomoX: trading trieDB.Commit(shutdown) failed", "root", root, "err", err)
			}
			tradingTrieDB.Dereference(root)
		}
	}

	// Flush all remaining lending trie roots to LevelDB.
	if sp.lendingEngine != nil {
		lendingTrieDB := sp.lendingEngine.GetStateCache().TrieDB()
		for !sp.lendingTriegc.Empty() {
			root := sp.lendingTriegc.PopItem().(common.Hash)
			if err := lendingTrieDB.Commit(root, true, nil); err != nil {
				log.Error("TomoZ: lending trieDB.Commit(shutdown) failed", "root", root, "err", err)
			}
			lendingTrieDB.Dereference(root)
		}
	}
}

// SetTradingEngine injects the TomoX trading engine into the block processor.
func (bc *BlockChain) SetTradingEngine(engine TradingEngine) {
	sp, ok := bc.processor.(*StateProcessor)
	if !ok {
		log.Error("SetTradingEngine: processor is not a *StateProcessor, trading engine not installed")
		return
	}
	sp.SetTradingEngine(engine)
	log.Info("TomoX trading engine installed on state processor")
}

// SetLendingEngine injects the TomoZ lending engine into the block processor.
func (bc *BlockChain) SetLendingEngine(engine LendingEngine) {
	sp, ok := bc.processor.(*StateProcessor)
	if !ok {
		log.Error("SetLendingEngine: processor is not a *StateProcessor, lending engine not installed")
		return
	}
	sp.SetLendingEngine(engine)
	log.Info("TomoZ lending engine installed on state processor")
}

// beforeProcessViction runs TomoZ liquidation data at epoch boundaries before
// the main transaction loop. Only active for pre-Atlas lending-enabled blocks.
func (bc *BlockChain) beforeProcessViction(block *types.Block, statedb *state.StateDB) error {
	if bc.chainConfig.Posv == nil {
		return nil
	}
	sp, ok := bc.processor.(*StateProcessor)
	if !ok || sp.lendingEngine == nil || sp.tradingEngine == nil {
		return nil
	}
	if !bc.chainConfig.IsTomoXLendingEnabled(block.Number()) {
		return nil
	}
	if block.NumberU64()%bc.chainConfig.Posv.Epoch != uint64(bc.chainConfig.Viction.LendingLiquidateTradeBlock) {
		return nil
	}

	parent := bc.GetBlock(block.ParentHash(), block.NumberU64()-1)
	if parent == nil {
		return nil
	}
	parentAuthor, err := bc.Engine().Author(parent.Header())
	if err != nil {
		return fmt.Errorf("TomoZ: liquidation: failed to resolve parent author: %w", err)
	}
	tradingState, err := sp.tradingEngine.GetTradingState(parent, parentAuthor)
	if err != nil {
		return fmt.Errorf("TomoZ: liquidation: failed to open TradingStateDB: %w", err)
	}
	lendingState, err := sp.lendingEngine.GetLendingState(parent, parentAuthor)
	if err != nil {
		return fmt.Errorf("TomoZ: liquidation: failed to open LendingStateDB: %w", err)
	}

	_, _, _, _, _, err = sp.lendingEngine.ProcessLiquidationData(
		block.Header(), bc, statedb, tradingState, lendingState,
	)
	if err != nil {
		return fmt.Errorf("TomoZ: ProcessLiquidationData failed at block %d: %w", block.NumberU64(), err)
	}
	log.Debug("TomoZ: epoch liquidation processed", "block", block.NumberU64())
	return nil
}

func (bc *BlockChain) UpdateM1() error {
	engine, ok := bc.Engine().(*posv.Posv)
	if bc.Config().Posv == nil || !ok {
		return fmt.Errorf("PoSV engine is not enabled")
	}
	log.Info("It's time to update new set of masternodes for the next epoch...")

	contractAddress := bc.chainConfig.Viction.ValidatorContract
	if contractAddress == (common.Address{}) {
		return fmt.Errorf("validator contract address is not set in chain config")
	}

	var candidates []common.Address

	// get candidates from slot of stateDB
	// if can't get anything, request from contracts
	stateDB, err := bc.State()
	if err != nil {
		return fmt.Errorf("failed to get state at current root (block %v): %v", bc.CurrentHeader().Number, err)
	}
	candidates = stateDB.VicGetCandidates(contractAddress)

	var ms []posv.Masternode
	for _, candidate := range candidates {
		_, cap := stateDB.VicGetValidatorInfo(contractAddress, candidate)

		//TODO: smart contract shouldn't return "0x0000000000000000000000000000000000000000"
		if candidate.String() != "0x0000000000000000000000000000000000000000" {
			ms = append(ms, posv.Masternode{Address: candidate, Stake: cap})
		}
	}
	if len(ms) == 0 {
		log.Error("No masternode found. Stopping node")
		return fmt.Errorf("no masternode found")
	} else {
		header := bc.CurrentHeader()
		if bc.Config().IsAtlas(header.Number) {
			sort.SliceStable(ms, func(i, j int) bool {
				return ms[i].Stake.Cmp(ms[j].Stake) >= 0
			})
		} else {
			// Must sort `ms`, not `candidates`: indices i,j are in [0, len(slice));
			// len(candidates) can exceed len(ms) when zero-address entries are skipped.
			sortlgc.Slice(ms, func(i, j int) bool {
				return ms[i].Stake.Cmp(ms[j].Stake) >= 0
			})
		}
		log.Info("Ordered list of masternode candidates")
		for _, m := range ms {
			log.Info("", "address", m.Address.String(), "stake", m.Stake)
		}
		// update masternodes
		log.Info("Updating new set of masternodes")
		if len(ms) > int(bc.chainConfig.Viction.ValidatorMaxCount) {
			err = engine.UpdateMasternodes(bc, header, ms[:bc.chainConfig.Viction.ValidatorMaxCount])
		} else {
			err = engine.UpdateMasternodes(bc, header, ms)
		}
		if err != nil {
			return err
		}
		log.Info("Masternodes are ready for the next epoch")
	}
	return nil
}
