package types

import (
	"bytes"

	"github.com/ethereum/go-ethereum/common"
)

// signMethodSelector is the 4-byte function selector for sign(uint256,bytes32).
var signMethodSelector = common.Hex2Bytes("e341eaa4")

// IsTomoXApplyTransaction returns true if the transaction is directed to the given TomoXContract address.
func (tx *Transaction) IsTomoXApplyTransaction(contractAddress common.Address) bool {
	if tx.To() == nil {
		return false
	}
	return *tx.To() == contractAddress
}

// IsSkipNonceTransaction returns true if the transaction is a system transaction that skips nonce checking.
func (tx *Transaction) IsSkipNonceTransaction(contractAddress common.Address) bool {
	return tx.IsTomoXApplyTransaction(contractAddress)
}

// IsTradingTransaction checks if a target address is one of the designated system exchange contracts
func IsTradingTransaction(to *common.Address) bool {
	if to == nil {
		return false
	}

	addr := *to
	return addr == common.HexToAddress("0x0000000000000000000000000000000000000091") || // TomoXContract
		addr == common.HexToAddress("0x0000000000000000000000000000000000000092") || // TradingState
		addr == common.HexToAddress("0x0000000000000000000000000000000000000093") || // Lending
		addr == common.HexToAddress("0x0000000000000000000000000000000000000094") // LendingFinalized
}

// IsSigningTransaction returns true if the transaction is a block-signer
// registration transaction to the BlockSigner contract.
// blockSignAddr is the ValidatorBlockSignContract address from chain config.
func (tx *Transaction) IsSigningTransaction(blockSignAddr common.Address) bool {
	if tx.To() == nil {
		return false
	}
	if *tx.To() != blockSignAddr {
		return false
	}
	data := tx.Data()
	if len(data) < 4 {
		return false
	}
	if !bytes.Equal(data[0:4], signMethodSelector) {
		return false
	}
	// sign(uint256 blockNumber, bytes32 blockHash) = 4 + 32 + 32 = 68 bytes
	if len(data) != 68 {
		return false
	}
	return true
}
