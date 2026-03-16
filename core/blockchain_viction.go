// Copyright 2026 The Vic-geth Authors
// This file provides vic-extensions to the geth BlockChain.
package core

import "github.com/ethereum/go-ethereum/log"

// SetTradingEngine injects the legacy TomoX trading engine into the block processor.
// This enables historical block replay for pre-Atlas TomoX transactions.
func (bc *BlockChain) SetTradingEngine(engine TradingEngine) {
	if sp, ok := bc.processor.(*StateProcessor); ok {
		sp.SetTradingEngine(engine)
		log.Info("TomoX trading engine set on state processor")
	}
}
