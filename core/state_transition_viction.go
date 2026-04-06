// Copyright 2026 The Vic-geth Authors
package core

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/vrc25"
)

// vrc25BuyGas checks VRC25 sponsorship eligibility and adjusts payer/gasPrice.
func (st *StateTransition) vrc25BuyGas() error {
	st.payer = st.msg.From()

	victionConfig := st.evm.ChainConfig().Viction
	if victionConfig == nil || victionConfig.VRC25Contract == (common.Address{}) {
		return nil
	}

	feeCap := vrc25.GetFeeCapacity(st.state, victionConfig.VRC25Contract, st.msg.To())
	if feeCap == nil || feeCap.Sign() == 0 {
		return nil
	}

	blockNum := st.evm.Context.BlockNumber

	if !st.evm.ChainConfig().IsAtlas(blockNum) {
		var effectiveGasPrice *big.Int
		if st.evm.ChainConfig().IsTIPTRC21Fee(blockNum) {
			effectiveGasPrice = (*big.Int)(victionConfig.VRC25GasPrice) // 250,000,000
		} else {
			effectiveGasPrice = (*big.Int)(victionConfig.TRC21GasPrice) // 2,500
		}

		mgval := new(big.Int).Mul(new(big.Int).SetUint64(st.msg.Gas()), effectiveGasPrice)
		if feeCap.Cmp(mgval) < 0 {
			return nil
		}
		// Set payer = VRC25Contract so isVRC25Transaction() returns true.
		// buyGas will skip the balance check and SubBalance for pre-Atlas sponsored txs.
		st.gasPrice = effectiveGasPrice
		st.payer = victionConfig.VRC25Contract
		return nil
	}

	vrc25GasPrice := (*big.Int)(victionConfig.VRC25GasPrice)
	vrc25GasFee := new(big.Int).Mul(new(big.Int).SetUint64(st.msg.Gas()), vrc25GasPrice)
	if feeCap.Cmp(vrc25GasFee) <= 0 {
		return nil
	}

	// Deduct storage slot upfront (vrc25PayGas equivalent).
	newFeeCap := new(big.Int).Sub(feeCap, vrc25GasFee)
	vrc25.SetFeeCapacity(st.state, victionConfig.VRC25Contract, *st.msg.To(), newFeeCap)

	st.gasPrice = vrc25GasPrice
	st.payer = victionConfig.VRC25Contract
	return nil
}

func (st *StateTransition) isVRC25Transaction() bool {
	return st.payer != st.msg.From()
}

// vrc25RefundGas handles gas refund for sponsored transactions.
//
// Pre-Atlas sponsored: victionchain refundGas does nothing when balanceFee != nil
// (state_transition.go:349-355). No native balance change, no storage restore.
//
// Post-Atlas sponsored: restore unused feeCap to the storage slot and credit native
// balance back to the issuer contract (vrc25RefundGas, state_transition.go:384-395).
func (st *StateTransition) vrc25RefundGas(remaining *big.Int) {
	if st.isVRC25Transaction() {
		blockNum := st.evm.Context.BlockNumber
		if !st.evm.ChainConfig().IsAtlas(blockNum) {
			// Pre-Atlas: nothing — victionchain does not touch any balance or storage
			// on refund when balanceFee != nil (state_transition.go:349-355).
			return
		}

		// Post-Atlas: restore unused feeCap to storage and native balance.
		addr := st.msg.To()
		victionConfig := st.evm.ChainConfig().Viction
		vrc25Contract := victionConfig.VRC25Contract
		feeCap := vrc25.GetFeeCapacity(st.state, vrc25Contract, addr)
		if feeCap != nil {
			storageRemaining := new(big.Int).Mul(
				new(big.Int).SetUint64(st.gas),
				(*big.Int)(victionConfig.VRC25GasPrice),
			)
			vrc25.SetFeeCapacity(st.state, vrc25Contract, *addr, new(big.Int).Add(feeCap, storageRemaining))
		}
		// Refund remaining gas value to the issuer contract (payer)
		st.state.AddBalance(st.payer, remaining)
		return
	}

	// Non-VRC25 transaction: refund remaining gas to the sender.
	st.state.AddBalance(st.msg.From(), remaining)
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

	txFee := new(big.Int).Mul(new(big.Int).SetUint64(st.gasUsed()), st.gasPrice)

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
		st.state.AddBalance(st.evm.Context.Coinbase, txFee)
		return
	}

	// After TIPTRC21Fee fork: route fee to the registered owner of the validator.
	slot := state.StorageLocationOfValidatorOwner(st.evm.Context.Coinbase)
	ownerHash := st.state.GetState(victionCfg.ValidatorContract, slot)
	owner := common.BytesToAddress(ownerHash.Bytes())
	if owner != (common.Address{}) {
		st.state.AddBalance(owner, txFee)
	}
}
