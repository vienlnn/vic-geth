// Copyright 2026 The Vic-geth Authors
package core

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/vrc25"
	"github.com/ethereum/go-ethereum/log"
)

var slotTokensState = vrc25.SlotVRC25Contract["tokensState"]

// buyVRC25Gas checks sponsorship eligibility and deducts the gas fee from the sponsor's storage balance.
func (st *StateTransition) vrc25BuyGas() error {
	// Default payer is the sender
	st.payer = st.msg.From()

	// 1. Check if contract is sponsored (has fee capacity)
	feeCap := vrc25.GetFeeCapacity(st.state, st.evm.ChainConfig().Viction.VRC25Contract, st.msg.To())
	if feeCap == nil {
		return nil // Not sponsored, proceed with standard user payment
	}
	victionConfig := st.evm.ChainConfig().Viction
	feeCapBefore := new(big.Int).Set(feeCap)

	// 2. Calculate Gas Cost with VRC25 Gas Price
	vrc25GasFee := new(big.Int).Mul(new(big.Int).SetUint64(st.msg.Gas()), (*big.Int)(victionConfig.VRC25GasPrice))

	// 3. Check sufficiency
	if feeCap.Cmp(vrc25GasFee) < 0 {
		return nil // Insufficient sponsor balance, fallback to user payment
	}

	// 4. Deduct from Contract's Storage Balance
	// Note: The native ETH deduction happens in state_transition.go via st.state.SubBalance(st.payer)
	newFeeCap := new(big.Int).Sub(feeCap, vrc25GasFee)
	feeCapKey := state.StorageLocationOfMappingElement(state.StorageLocationFromSlot(slotTokensState), st.msg.To().Hash().Bytes())
	st.state.SetState(victionConfig.VRC25Contract, feeCapKey.Hash(), common.BigToHash(newFeeCap))

	// 5. Set Payer to System Contract
	// This ensures buyGas() deducts native ETH from the system contract
	st.gasPrice = (*big.Int)(victionConfig.VRC25GasPrice)
	st.payer = victionConfig.VRC25Contract
	log.Debug("VRC25 sponsorship selected",
		"from", st.msg.From().Hex(),
		"to", addressPtrHex(st.msg.To()),
		"payer", st.payer.Hex(),
		"feeCapBefore", feeCapBefore.String(),
		"feeCapAfter", newFeeCap.String(),
		"vrc25GasFee", vrc25GasFee.String(),
		"vrc25GasPrice", st.gasPrice.String(),
	)

	return nil
}

func (st *StateTransition) isVRC25Transaction() bool {
	return st.payer != st.msg.From()
}

// vrc25RefundGas is called for VRC25-sponsored transactions and all Atlas-block transactions.
// For sponsored transactions it also restores the unused portion to the fee capacity storage slot.
// For non-sponsored Atlas transactions it only returns native ETH to the sender.
func (st *StateTransition) vrc25RefundGas(remaining *big.Int) {
	if st.isVRC25Transaction() {
		addr := st.msg.To()
		vrc25Contract := st.evm.ChainConfig().Viction.VRC25Contract
		feeCap := vrc25.GetFeeCapacity(st.state, vrc25Contract, addr)
		if feeCap != nil { // always non-nil for non-nil addr; guard defensively
			newFeeCap := new(big.Int).Add(feeCap, remaining)
			feeCapKey := state.StorageLocationOfMappingElement(state.StorageLocationFromSlot(slotTokensState), addr.Hash().Bytes())
			st.state.SetState(vrc25Contract, feeCapKey.Hash(), common.BigToHash(newFeeCap))
			log.Debug("VRC25 refund to fee capacity",
				"to", addressPtrHex(addr),
				"remaining", remaining.String(),
				"feeCapBefore", feeCap.String(),
				"feeCapAfter", newFeeCap.String(),
			)
		}
	}

	// Return native ETH to the payer (VRC25Contract for sponsored txs, sender otherwise).
	payerBefore := new(big.Int).Set(st.state.GetBalance(st.payer))
	st.state.AddBalance(st.payer, remaining)
	if st.isVRC25Transaction() {
		log.Debug("VRC25 native gas refund",
			"payer", st.payer.Hex(),
			"remaining", remaining.String(),
			"payerBalanceBefore", payerBefore.String(),
			"payerBalanceAfter", st.state.GetBalance(st.payer).String(),
		)
	}
}

// applyTransactionFee distributes the transaction fee to the correct recipient.
//
// After the TIPTRC21Fee fork the fee goes to the validator-owner stored on-chain
// inside VictionConfig.ValidatorContract. Before that fork, or when no owner is
// registered, the fee falls back to the block coinbase.
//
// When the Atlas fork is active and this is a VRC25-sponsored transaction the fee
// amount is re-derived using VictionConfig.VRC25GasPrice (which matches the price
// already used in buyGas / refundGas) instead of the regular gasPrice.
func (st *StateTransition) applyTransactionFee() {
	victionCfg := st.evm.ChainConfig().Viction
	blockNum := st.evm.Context.BlockNumber

	txFee := new(big.Int).SetUint64(st.gasUsed())
	if st.evm.ChainConfig().IsTIPTRC21Fee(blockNum) {
		txFee = new(big.Int).Mul(new(big.Int).SetUint64(st.gasUsed()), st.gasPrice)
	}
	if victionCfg == nil {
		// Non-Viction chain: fee always goes to the coinbase.
		st.state.AddBalance(st.evm.Context.Coinbase, txFee)
		return
	}

	// After Atlas HF, VRC25-sponsored transactions carry a different gas price that
	// was set on st.gasPrice in vrc25BuyGas. However, if IsAtlas and we are a VRC25
	// transaction the gasPrice was already overridden to VRC25GasPrice, so txFee is
	// already correct. Explicitly recalculate only when VRC25GasPrice is set and the
	// current gasPrice could have been overridden (i.e., IsAtlas is active).
	if st.evm.ChainConfig().IsAtlas(blockNum) && st.isVRC25Transaction() && victionCfg.VRC25GasPrice != nil {
		txFee = new(big.Int).Mul(new(big.Int).SetUint64(st.gasUsed()), (*big.Int)(victionCfg.VRC25GasPrice))
	}

	// Before TIPTRC21Fee fork: fee goes to the block coinbase.
	if !st.evm.ChainConfig().IsTIPTRC21Fee(blockNum) {
		coinbaseBefore := new(big.Int).Set(st.state.GetBalance(st.evm.Context.Coinbase))
		st.state.AddBalance(st.evm.Context.Coinbase, txFee)
		if st.isVRC25Transaction() {
			log.Debug("VRC25 fee to coinbase",
				"coinbase", st.evm.Context.Coinbase.Hex(),
				"txFee", txFee.String(),
				"coinbaseBalanceBefore", coinbaseBefore.String(),
				"coinbaseBalanceAfter", st.state.GetBalance(st.evm.Context.Coinbase).String(),
			)
		}
		return
	}

	// After TIPTRC21Fee fork: route fee to the registered owner of the validator.
	slot := state.StorageLocationOfValidatorOwner(st.evm.Context.Coinbase)
	ownerHash := st.state.GetState(victionCfg.ValidatorContract, slot)
	owner := common.BytesToAddress(ownerHash.Bytes())
	if owner != (common.Address{}) {
		ownerBefore := new(big.Int).Set(st.state.GetBalance(owner))
		st.state.AddBalance(owner, txFee)
		if st.isVRC25Transaction() {
			log.Debug("VRC25 fee to validator owner",
				"owner", owner.Hex(),
				"coinbase", st.evm.Context.Coinbase.Hex(),
				"txFee", txFee.String(),
				"ownerBalanceBefore", ownerBefore.String(),
				"ownerBalanceAfter", st.state.GetBalance(owner).String(),
			)
		}
	} else if st.isVRC25Transaction() {
		log.Debug("VRC25 fee recipient missing owner",
			"coinbase", st.evm.Context.Coinbase.Hex(),
			"validatorContract", victionCfg.ValidatorContract.Hex(),
			"txFee", txFee.String(),
		)
	}
}
