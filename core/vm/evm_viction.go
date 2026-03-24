package vm

import (
	"errors"

	"github.com/ethereum/go-ethereum/common"
)

func (evm *EVM) isPosvEngine() bool {
	return evm.chainConfig != nil && evm.chainConfig.Posv != nil
}

// precompileViction selects precompiles with Viction fork gating.
// Istanbul precompiles are enabled only after TIPTomoXCancelFee.
func (evm *EVM) precompileViction(addr common.Address) (PrecompiledContract, bool) {
	var precompiles map[common.Address]PrecompiledContract
	switch {
	case evm.chainRules.IsYoloV2:
		precompiles = PrecompiledContractsYoloV2
	case evm.chainRules.IsIstanbul && evm.ChainConfig().IsTIPTomoXCancelFee(evm.Context.BlockNumber):
		precompiles = PrecompiledContractsIstanbul
	case evm.chainRules.IsByzantium:
		precompiles = PrecompiledContractsByzantium
	default:
		precompiles = PrecompiledContractsHomestead
	}
	p, ok := precompiles[addr]
	return p, ok
}

func (evm *EVM) precompileByEngine(addr common.Address) (PrecompiledContract, bool) {
	if evm.isPosvEngine() {
		return evm.precompileViction(addr)
	}
	return evm.precompile(addr)
}

// precompileMapForCallByEngine is only for EIP-158 empty-account touch handling in Call.
func (evm *EVM) precompileMapForCallByEngine() map[common.Address]PrecompiledContract {
	allowIstanbul := evm.chainRules.IsIstanbul
	if evm.isPosvEngine() {
		// POSV differs only here: Istanbul precompiles are enabled at TIPTomoXCancelFee.
		allowIstanbul = allowIstanbul && evm.ChainConfig().IsTIPTomoXCancelFee(evm.Context.BlockNumber)
	}

	switch {
	case evm.chainRules.IsYoloV2:
		return PrecompiledContractsYoloV2
	case allowIstanbul:
		return PrecompiledContractsIstanbul
	case evm.chainRules.IsByzantium:
		return PrecompiledContractsByzantium
	default:
		return PrecompiledContractsHomestead
	}
}

// runViction executes contract code with Viction-specific fork gating.
// Before TIPTomoXCancelFee, it uses the default interpreter directly.
// From TIPTomoXCancelFee onward, it follows the standard interpreter selection flow.
func runViction(evm *EVM, contract *Contract, input []byte, readOnly bool) ([]byte, error) {
	if evm.ChainConfig().IsTIPTomoXCancelFee(evm.Context.BlockNumber) {
		for _, interpreter := range evm.interpreters {
			if interpreter.CanRun(contract.Code) {
				if evm.interpreter != interpreter {
					// Ensure that the interpreter pointer is set back
					// to its current value upon return.
					defer func(i Interpreter) {
						evm.interpreter = i
					}(evm.interpreter)
					evm.interpreter = interpreter
				}
				return interpreter.Run(contract, input, readOnly)
			}
		}

	} else {
		return evm.interpreter.Run(contract, input, readOnly)
	}
	return nil, errors.New("no compatible interpreter")
}

func runByEngine(evm *EVM, contract *Contract, input []byte, readOnly bool) ([]byte, error) {
	if evm.isPosvEngine() {
		return runViction(evm, contract, input, readOnly)
	}
	return run(evm, contract, input, readOnly)
}
