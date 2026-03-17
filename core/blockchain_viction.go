// Copyright 2026 The Vic-geth Authors
// This file provides vic-extensions to the geth BlockChain.
package core

import (
	"fmt"
	"sort"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/posv"
	"github.com/ethereum/go-ethereum/log"
	"github.com/tforce-io/tf-golib/stdx/mathxt/bigxt"
)

// SetTradingEngine injects the legacy TomoX trading engine into the block processor.
// This enables historical block replay for pre-Atlas TomoX transactions.
func (bc *BlockChain) SetTradingEngine(engine TradingEngine) {
	sp, ok := bc.processor.(*StateProcessor)
	if !ok {
		log.Error("SetTradingEngine: processor is not a *StateProcessor, trading engine not installed")
		return
	}
	sp.SetTradingEngine(engine)
	log.Info("TomoX trading engine installed on state processor")
}

func (bc *BlockChain) UpdateM1() error {
	engine, ok := bc.Engine().(*posv.Posv)
	if bc.Config().Posv == nil || !ok {
		return fmt.Errorf("PoSV engine is not enabled")
	}
	log.Info("It's time to update new set of masternodes for the next epoch...")

	contracrAddress := bc.chainConfig.Viction.ValidatorContract
	if contracrAddress == (common.Address{}) {
		return fmt.Errorf("validator contract address is not set in chain config")
	}

	var candidates []common.Address

	// get candidates from slot of stateDB
	// if can't get anything, request from contracts
	stateDB, err := bc.State()
	if err != nil {
		return fmt.Errorf("failed to get state at current root (block %v): %v", bc.CurrentHeader().Number, err)
	}
	candidates = stateDB.VicGetCandidates(contracrAddress)

	var ms []posv.Masternode
	for _, candidate := range candidates {
		_, cap := stateDB.VicGetValidatorInfo(contracrAddress, candidate)

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
			sort.Slice(candidates, func(i, j int) bool {
				return bigxt.IsGreaterThanOrEqualInt(ms[i].Stake, ms[j].Stake)
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
