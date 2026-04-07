// Copyright 2026 The Vic-geth Authors
package core

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/prque"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/legacy/tomox/tradingstate"
	"github.com/ethereum/go-ethereum/legacy/tomoxlending/lendingstate"
)

// LendingEngine is the interface the TomoZ lending engine must satisfy.
// Defined here to avoid an import cycle between core and legacy/tomoxlending.
type LendingEngine interface {
	GetLendingStateRoot(block *types.Block, author common.Address) (common.Hash, error)
	GetLendingState(block *types.Block, author common.Address) (*lendingstate.LendingStateDB, error)
	HasLendingState(block *types.Block, author common.Address) bool
	GetStateCache() lendingstate.Database
	GetTriegc() *prque.Prque
	CommitOrder(
		header *types.Header,
		coinbase common.Address,
		chain tradingstate.ChainContext,
		statedb *state.StateDB,
		lendingStateDB *lendingstate.LendingStateDB,
		tradingStateDB *tradingstate.TradingStateDB,
		lendingOrderBook common.Hash,
		order *lendingstate.LendingItem,
	) ([]*lendingstate.LendingTrade, []*lendingstate.LendingItem, error)
	GetCollateralPrices(
		header *types.Header,
		chain tradingstate.ChainContext,
		statedb *state.StateDB,
		tradingStateDB *tradingstate.TradingStateDB,
		collateralToken common.Address,
		lendingToken common.Address,
	) (*big.Int, *big.Int, error)
	GetMediumTradePriceBeforeEpoch(
		chain tradingstate.ChainContext,
		statedb *state.StateDB,
		tradingStateDB *tradingstate.TradingStateDB,
		baseToken common.Address,
		quoteToken common.Address,
	) (*big.Int, error)
	ProcessLiquidationData(
		header *types.Header,
		chain tradingstate.ChainContext,
		statedb *state.StateDB,
		tradingState *tradingstate.TradingStateDB,
		lendingState *lendingstate.LendingStateDB,
	) (
		updatedTrades map[common.Hash]*lendingstate.LendingTrade,
		liquidatedTrades []*lendingstate.LendingTrade,
		autoRepayTrades []*lendingstate.LendingTrade,
		autoTopUpTrades []*lendingstate.LendingTrade,
		autoRecallTrades []*lendingstate.LendingTrade,
		err error,
	)
}
