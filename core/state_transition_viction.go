// Copyright 2026 The Vic-geth Authors
package core

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/vrc25"
)

// vrc25BuyGas checks VRC25 sponsorship eligibility and adjusts payer/gasPrice.
//
// Pre-Atlas: victionchain overrides msg.gasPrice in AsMessage (types/transaction.go:264-271)
// to TRC21GasPriceBefore (2500) pre-TIPTRC21Fee, or TRC21GasPrice (250M) post-TIPTRC21Fee.
// checkBalance then tests feeCap >= gasLimit × that overridden price. subtractBalance does
// nothing when balanceFee != nil — no native balance or storage touched during the tx.
// We replicate this by overriding st.gasPrice so eligibility and coinbase fee use the right
// price, but we leave payer = sender so upstream SubBalance hits the sender (who has enough
// balance because vrc25BuyGas pre-credits the sender to cover it).
//
// Post-Atlas: feeCap must strictly exceed gasLimit × VRC25GasPrice. vrc25PayGas writes the
// storage slot and SubBalance inside the same call, so we set payer = VRC25Contract and let
// upstream SubBalance handle the native deduction (no pre-credit needed).
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
		// Pre-Atlas: eligibility uses the era-overridden gas price, not the user-submitted one.
		// victionchain AsMessage sets gasPrice to TRC21GasPriceBefore (2500) pre-TIPTRC21Fee
		// and TRC21GasPrice (250M) post-TIPTRC21Fee (types/transaction.go:264-271).
		var effectiveGasPrice *big.Int
		if st.evm.ChainConfig().IsTIPTRC21Fee(blockNum) {
			effectiveGasPrice = (*big.Int)(victionConfig.VRC25GasPrice) // 250,000,000
		} else {
			effectiveGasPrice = (*big.Int)(victionConfig.TRC21GasPrice) // 2,500
		}

		mgval := new(big.Int).Mul(new(big.Int).SetUint64(st.msg.Gas()), effectiveGasPrice)
		// victionchain checkBalance pre-Atlas: balanceTokenFee.Cmp(mgval) < 0 → reject
		// so sponsorship requires feeCap >= mgval.
		if feeCap.Cmp(mgval) < 0 {
			return nil
		}

		// Override gasPrice so coinbase fee and refund use the correct era price.
		// Payer stays as sender; pre-credit sender so upstream SubBalance nets to zero.
		st.gasPrice = effectiveGasPrice
		st.state.AddBalance(st.msg.From(), mgval)
		return nil
	}

	// Post-Atlas: victionchain checkBalance uses Cmp(vrc25val) <= 0 → not sponsored.
	// Sponsorship requires feeCap > gasLimit × VRC25GasPrice (strictly greater).
	vrc25GasPrice := (*big.Int)(victionConfig.VRC25GasPrice)
	vrc25GasFee := new(big.Int).Mul(new(big.Int).SetUint64(st.msg.Gas()), vrc25GasPrice)
	if feeCap.Cmp(vrc25GasFee) <= 0 {
		return nil
	}

	// Deduct storage slot (vrc25PayGas equivalent for the pre-execution part).
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
	}

	st.state.AddBalance(st.payer, remaining)
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
