// Copyright 2026 The Vic-geth Authors
package state

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

func GetStorageKeyForSlot(slot uint64) common.Hash {
	slotHash := common.BigToHash(new(big.Int).SetUint64(slot))
	return slotHash
}

func GetStorageKeyForMapping(key common.Hash, slot uint64) common.Hash {
	slotHash := common.BigToHash(new(big.Int).SetUint64(slot))
	retByte := crypto.Keccak256(key.Bytes(), slotHash.Bytes())
	ret := new(big.Int)
	ret.SetBytes(retByte)
	return common.BigToHash(ret)
}

// GetLocDynamicArrAtElement is used to get the location of element inside dynamic array
func GetLocDynamicArrAtElement(slotHash common.Hash, index uint64, elementSize uint64) common.Hash {
	slotKecBig := crypto.Keccak256Hash(slotHash.Bytes()).Big()
	// arrBig = slotKecBig + index * elementSize
	arrBig := slotKecBig.Add(slotKecBig, new(big.Int).SetUint64(index*elementSize))
	return common.BigToHash(arrBig)
}
