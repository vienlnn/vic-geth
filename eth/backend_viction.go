package eth

import (
	"fmt"
	"math/big"
	"sort"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/posv"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/viction"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/legacy/tomox"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/params"
	"github.com/tforce-io/tf-golib/stdx/mathxt/bigxt"
)

const SignMethodHex = "e341eaa4"

// Get attestors from list of validators at checkpoint block.
func (s *Ethereum) PosvGetAttestors(vicConfig *params.VictionConfig, header *types.Header, validators []common.Address,
) ([]int64, error) {
	state, err := s.BlockChain().StateAt(header.Root)
	if err != nil {
		return nil, err
	}
	return viction.GetAttestors(vicConfig, validators, state)
}

// Get block signers from the state.
func (s *Ethereum) PosvGetBlockSignData(config *params.ChainConfig, vicConfig *params.VictionConfig, header *types.Header,
	chain consensus.ChainReader,
) []types.Transaction {
	blockNumber := header.Number
	block := chain.GetBlock(header.Hash(), blockNumber.Uint64())
	if block == nil {
		return []types.Transaction{}
	}
	data := []types.Transaction{}

	// Before TIPSigning, block-sign txs are EVM-executed and may fail. Only
	// successful signing txs count toward rewards and penalties.
	var receipts types.Receipts
	if !config.IsTIPSigning(blockNumber) {
		receipts = s.blockchain.GetReceiptsByHash(header.Hash())
	}

	for i, tx := range block.Transactions() {
		if !tx.IsSigningTransaction(vicConfig.ValidatorBlockSignContract) {
			continue
		}
		if receipts != nil && i < len(receipts) {
			r := receipts[i]
			var status uint64
			if len(r.PostState) > 0 {
				status = types.ReceiptStatusSuccessful
			} else {
				status = r.Status
			}
			if status == types.ReceiptStatusFailed {
				continue
			}
		}
		data = append(data, *tx)
	}
	return data
}

// Get creator-attestor pairs from the state.
func (s *Ethereum) PosvGetCreatorAttestorPairs(c *posv.Posv, config *params.ChainConfig,
	header, checkpointHeader *types.Header,
) (map[common.Address]common.Address, uint64, error) {
	return viction.GetCreatorAttestorPairs(c, config, config.Posv, header, checkpointHeader)
}

// PosvGetEpochReward calculates and distributes reward at checkpoint block.
func (s *Ethereum) PosvGetEpochReward(c *posv.Posv, config *params.ChainConfig, posvConfig *params.PosvConfig, vicConfig *params.VictionConfig,
	header *types.Header,
	chain consensus.ChainReader, statedb *state.StateDB, logger log.Logger,
) (*posv.EpochReward, error) {
	epochRewards := &posv.EpochReward{}
	blockNumber := header.Number.Uint64()

	// Skip block 900 (1*epoch); first reward at block 1800 (2*epoch)
	if blockNumber <= posvConfig.Epoch {
		return epochRewards, nil
	}

	// Get initial reward
	initialRewardPerEpoch := (*big.Int)(vicConfig.RewardPerEpoch)
	totalReward := viction.CalcDefaultRewardPerBlock(initialRewardPerEpoch, blockNumber, posvConfig.BlocksPerYear())

	// Get additional reward for Saigon upgrade
	if config.IsSaigon(header.Number) && vicConfig.SaigonRewardPerEpoch != nil {
		saigonRewardPerEpoch := (*big.Int)(vicConfig.SaigonRewardPerEpoch)
		saigonReward := viction.CalcSaigonRewardPerBlock(saigonRewardPerEpoch, config.SaigonBlock, blockNumber, posvConfig.BlocksPerYear())
		totalReward = new(big.Int).Add(totalReward, saigonReward)
	}

	// Calculate rewards for validators and stakeholders
	validatorRewards, err := viction.CalcRewardsForValidators(c, config, posvConfig, vicConfig, header, totalReward, chain, logger)
	if err != nil {
		return nil, err
	}
	epochRewards.ValidatorRewards = validatorRewards

	// Use pre-transaction state for voter caps
	parentHeader := chain.GetHeader(header.ParentHash, blockNumber-1)
	var rewardState *state.StateDB
	if parentHeader != nil {
		rewardState, err = s.BlockChain().StateAt(parentHeader.Root)
		if err != nil {
			logger.Warn("PosvGetEpochReward: failed to get parent state, falling back to current state", "block", blockNumber, "err", err)
			rewardState = statedb
		}
	} else {
		rewardState = statedb
	}

	stakeholderRewards, err := viction.CalcRewardsForStakeholders(c, config, posvConfig, vicConfig, header, validatorRewards, rewardState, logger)
	if err != nil {
		return nil, err
	}
	epochRewards.StakholderRewards = stakeholderRewards

	return epochRewards, nil
}

// PosvAddBalanceRewards applies epoch rewards to the state by adding balances to all stakeholders.
// It does NOT recalculate; caller should pass the epochReward returned by PosvGetEpochReward.
func (s *Ethereum) PosvDistributeEpochRewards(header *types.Header, state *state.StateDB, epochReward *posv.EpochReward) error {
	blockNumber := header.Number.Uint64()

	if epochReward == nil {
		log.Debug("PosvAddBalanceRewards: no epoch rewards to apply", "block", blockNumber)
		return nil
	}
	if state == nil {
		return nil
	}

	// Apply stakeholder rewards to the state
	totalRewardDistributed := big.NewInt(0)
	rewardCount := 0

	for addr, amount := range epochReward.StakholderRewards {
		if amount == nil || amount.Sign() <= 0 {
			continue
		}
		state.AddBalance(addr, amount)
		totalRewardDistributed.Add(totalRewardDistributed, amount)
		rewardCount++
	}

	log.Info("PosvAddBalanceRewards: applied epoch rewards", "block", blockNumber, "recipientCount", rewardCount, "totalReward", totalRewardDistributed.String())
	return nil
}

// Get list of validators creating bad block or not creating block at all.
func (s *Ethereum) PosvGetPenalties(c *posv.Posv, config *params.ChainConfig, posvConfig *params.PosvConfig, vicConfig *params.VictionConfig,
	header *types.Header,
	chain consensus.ChainReader,
) ([]common.Address, error) {
	if config.IsTIPSigning(header.Number) {
		return viction.PenalizeValidatorsTIPSigning(c, config, posvConfig, vicConfig, header, chain)
	}
	return viction.PenalizeValidatorsDefault(c, config, posvConfig, vicConfig, header, chain)
}

// Get eligble validators from the state.
func (s *Ethereum) PosvGetValidators(vicConfig *params.VictionConfig, header *types.Header, chain consensus.ChainReader,
) ([]common.Address, error) {
	if header == nil {
		return []common.Address{}, nil
	}
	// Guard against being called before eth.blockchain is assigned (e.g. during
	// NewBlockChain's internal VerifyHeader when SetBackend was called too early).
	bc := s.BlockChain()
	if bc == nil {
		return []common.Address{}, fmt.Errorf("blockchain not initialized (block %v)", header.Number)
	}

	state, err := bc.StateAt(header.Root)
	if err != nil {
		return []common.Address{}, fmt.Errorf("failed to get state at header root (block %v): %v", header.Number, err)
	}
	contracrAddress := vicConfig.ValidatorContract
	if contracrAddress == (common.Address{}) {
		return []common.Address{}, viction.ErrNoContractAddress
	}
	addresses := state.VicGetCandidates(contracrAddress)
	candidates := []*posv.ValidatorInfo{}
	for _, addr := range addresses {
		if addr == (common.Address{}) {
			continue
		}
		_, cap := state.VicGetValidatorInfo(contracrAddress, addr)
		candidates = append(candidates, &posv.ValidatorInfo{Address: addr, Capacity: cap})
	}

	sort.Slice(candidates, func(i, j int) bool {
		return bigxt.IsGreaterThanOrEqualInt(candidates[i].Capacity, candidates[j].Capacity)
	})
	validatorMaxCountInt := int(vicConfig.ValidatorMaxCount)
	if len(candidates) > validatorMaxCountInt {
		candidates = candidates[:validatorMaxCountInt]
	}
	validators := []common.Address{}
	for _, candidate := range candidates {
		validators = append(validators, candidate.Address)
	}
	return validators, nil

}

func (s *Ethereum) PosvSetTomoxTradingEngine(tradingDb ethdb.Database) {
	tomoxEngine := tomox.NewWithDB(tradingDb)
	s.blockchain.SetTradingEngine(tomoxEngine)
}

// setupPosvBackend wires the POSV engine backend and optional TomoX trading engine.
func (eth *Ethereum) setupPosvBackend(chainConfig *params.ChainConfig, stack *node.Node) error {
	if chainConfig.Posv == nil {
		return nil
	}

	posvEngine, ok := eth.engine.(*posv.Posv)
	if !ok {
		return fmt.Errorf("posv config present but engine is %T, expected *posv.Posv", eth.engine)
	}

	// Wire POSV backend
	posvEngine.SetBackend(eth)
	log.Info("PosvBackend set on Posv engine")
	if head := eth.blockchain.CurrentHeader(); head != nil {
		if err := eth.engine.VerifyHeader(eth.blockchain, head, true); err != nil {
			log.Warn("Head invalid after full POSV check", "number", head.Number, "hash", head.Hash(), "err", err)
		}
	}

	// Initialize legacy TomoX trading engine for historical block replay.
	// Required on Viction (Posv) networks to sync pre-Atlas blocks containing
	// TomoX order matching transactions (0x91). Not needed on non-Posv chains.
	tradingDb, err := stack.OpenDatabase("tomox", 256, 256, "eth/db/tomox/")
	if err != nil {
		log.Error("Failed to open TomoX trading database", "err", err)
	} else {
		eth.PosvSetTomoxTradingEngine(tradingDb)
	}

	return nil
}
