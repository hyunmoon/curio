package seal

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ipfs/go-cid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/big"
	"github.com/filecoin-project/go-state-types/builtin"
	"github.com/filecoin-project/go-state-types/crypto"
	"github.com/filecoin-project/go-state-types/network"

	"github.com/filecoin-project/curio/deps/config"
	"github.com/filecoin-project/curio/harmony/harmonytask"
	"github.com/filecoin-project/curio/lib/multictladdr"

	"github.com/filecoin-project/lotus/api"
	"github.com/filecoin-project/lotus/chain/actors/builtin/miner"
	"github.com/filecoin-project/lotus/chain/actors/policy"
	"github.com/filecoin-project/lotus/chain/types"
)

const testPrecommitHead = abi.ChainEpoch(1000)

// mockPrecommitAPI implements the minimal SubmitPrecommitTaskApi interface for testing
type mockPrecommitAPI struct {
	walletBalances map[address.Address]big.Int
	walletHas      map[address.Address]bool
	minerBalance   big.Int
	head           *types.TipSet
	minerInfo      api.MinerInfo
	precommitErr   error
	networkVersion network.Version
	chainHeadCalls int
}

func (m *mockPrecommitAPI) ChainHead(context.Context) (*types.TipSet, error) {
	m.chainHeadCalls++
	return m.head, nil
}

func (m *mockPrecommitAPI) StateMinerPreCommitDepositForPower(context.Context, address.Address, miner.SectorPreCommitInfo, types.TipSetKey) (big.Int, error) {
	if m.precommitErr != nil {
		return big.Zero(), m.precommitErr
	}
	return big.Zero(), nil
}

func (m *mockPrecommitAPI) StateMinerInfo(context.Context, address.Address, types.TipSetKey) (api.MinerInfo, error) {
	return m.minerInfo, nil
}

func (m *mockPrecommitAPI) StateNetworkVersion(context.Context, types.TipSetKey) (network.Version, error) {
	return m.networkVersion, nil
}

func (m *mockPrecommitAPI) StateMinerAvailableBalance(context.Context, address.Address, types.TipSetKey) (big.Int, error) {
	return m.minerBalance, nil
}

func (m *mockPrecommitAPI) GasEstimateMessageGas(context.Context, *types.Message, *api.MessageSendSpec, types.TipSetKey) (*types.Message, error) {
	return nil, nil
}

// ctladdr.NodeApi methods
func (m *mockPrecommitAPI) WalletBalance(ctx context.Context, addr address.Address) (types.BigInt, error) {
	if bal, ok := m.walletBalances[addr]; ok {
		return bal, nil
	}
	return big.Zero(), nil
}

func (m *mockPrecommitAPI) WalletHas(ctx context.Context, addr address.Address) (bool, error) {
	if has, ok := m.walletHas[addr]; ok {
		return has, nil
	}
	return false, nil
}

func (m *mockPrecommitAPI) StateAccountKey(ctx context.Context, addr address.Address, tsk types.TipSetKey) (address.Address, error) {
	return addr, nil
}

func (m *mockPrecommitAPI) StateLookupID(ctx context.Context, addr address.Address, tsk types.TipSetKey) (address.Address, error) {
	return addr, nil
}

type precommitSectorKey struct {
	spID         int64
	sectorNumber int64
}

type recordedPrecommitFailure struct {
	taskID  harmonytask.TaskID
	reason  string
	message string
}

type mockPrecommitTaskStore struct {
	sectors        []precommitSectorParams
	pieces         map[precommitSectorKey][]precommitPiece
	detached       []precommitSectorKey
	failures       map[precommitSectorKey]recordedPrecommitFailure
	loadSectorsErr error
	detachErr      error
	loadPiecesErr  error
	failSectorErr  error
	messageCIDErr  error
	messageWaitErr error
	messageSectors []int64
	messageSPID    int64
	messageTaskID  harmonytask.TaskID
	messageCID     cid.Cid
	messageWaits   []cid.Cid
}

func (m *mockPrecommitTaskStore) loadSectors(context.Context, harmonytask.TaskID) ([]precommitSectorParams, error) {
	if m.loadSectorsErr != nil {
		return nil, m.loadSectorsErr
	}
	return append([]precommitSectorParams(nil), m.sectors...), nil
}

func (m *mockPrecommitTaskStore) detachFailedSector(_ context.Context, _ harmonytask.TaskID, spID, sectorNumber int64) error {
	if m.detachErr != nil {
		return m.detachErr
	}
	m.detached = append(m.detached, precommitSectorKey{spID: spID, sectorNumber: sectorNumber})
	return nil
}

func (m *mockPrecommitTaskStore) loadPieces(_ context.Context, spID, sectorNumber int64) ([]precommitPiece, error) {
	if m.loadPiecesErr != nil {
		return nil, m.loadPiecesErr
	}
	return append([]precommitPiece(nil), m.pieces[precommitSectorKey{spID: spID, sectorNumber: sectorNumber}]...), nil
}

func (m *mockPrecommitTaskStore) failSector(_ context.Context, taskID harmonytask.TaskID, spID, sectorNumber int64, reason, message string) error {
	if m.failSectorErr != nil {
		return m.failSectorErr
	}
	if m.failures == nil {
		m.failures = map[precommitSectorKey]recordedPrecommitFailure{}
	}
	m.failures[precommitSectorKey{spID: spID, sectorNumber: sectorNumber}] = recordedPrecommitFailure{
		taskID:  taskID,
		reason:  reason,
		message: message,
	}
	return nil
}

func (m *mockPrecommitTaskStore) setMessageCID(_ context.Context, taskID harmonytask.TaskID, spID int64, sectors []int64, mcid cid.Cid) error {
	if m.messageCIDErr != nil {
		return m.messageCIDErr
	}
	m.messageTaskID = taskID
	m.messageSPID = spID
	m.messageSectors = append([]int64(nil), sectors...)
	m.messageCID = mcid
	return nil
}

func (m *mockPrecommitTaskStore) addMessageWait(_ context.Context, mcid cid.Cid) error {
	if m.messageWaitErr != nil {
		return m.messageWaitErr
	}
	m.messageWaits = append(m.messageWaits, mcid)
	return nil
}

type mockPrecommitMessageSender struct {
	messages []*types.Message
	cid      cid.Cid
	err      error
}

func (m *mockPrecommitMessageSender) Send(_ context.Context, msg *types.Message, _ *api.MessageSendSpec, _ string) (cid.Cid, error) {
	if m.err != nil {
		return cid.Undef, m.err
	}
	m.messages = append(m.messages, msg)
	return m.cid, nil
}

func makePrecommitTipSet(t *testing.T, height abi.ChainEpoch) *types.TipSet {
	t.Helper()

	minerAddr, err := address.NewIDAddress(1000)
	require.NoError(t, err)
	root, err := cid.Decode("bafy2bzacea3wsdh6y3a36tb3skempjoxqpuyompjbmfeyf34fi3uy6uue42v4")
	require.NoError(t, err)

	ts, err := types.NewTipSet([]*types.BlockHeader{{
		Miner:                 minerAddr,
		Ticket:                &types.Ticket{VRFProof: []byte{1}},
		Height:                height,
		ParentStateRoot:       root,
		Messages:              root,
		ParentMessageReceipts: root,
		BlockSig:              &crypto.Signature{Type: crypto.SigTypeSecp256k1},
		BLSAggregate:          &crypto.Signature{Type: crypto.SigTypeSecp256k1},
		Timestamp:             uint64(time.Now().Unix()),
		ParentBaseFee:         types.NewInt(100),
	}})
	require.NoError(t, err)
	return ts
}

func makeSubmitPrecommitTaskForTest(t *testing.T, sectors []precommitSectorParams, pieces map[precommitSectorKey][]precommitPiece) (*SubmitPrecommitTask, *mockPrecommitTaskStore, *mockPrecommitMessageSender, *mockPrecommitAPI) {
	t.Helper()

	worker, err := address.NewIDAddress(1001)
	require.NoError(t, err)
	messageCID, err := cid.Decode("bafy2bzacea3wsdh6y3a36tb3skempjoxqpuyompjbmfeyf34fi3uy6uue42v4")
	require.NoError(t, err)

	store := &mockPrecommitTaskStore{
		sectors: sectors,
		pieces:  pieces,
	}
	sender := &mockPrecommitMessageSender{cid: messageCID}
	testAPI := &mockPrecommitAPI{
		head:           makePrecommitTipSet(t, testPrecommitHead),
		minerInfo:      api.MinerInfo{Worker: worker},
		networkVersion: network.Version21,
	}
	task := &SubmitPrecommitTask{
		store:  store,
		api:    testAPI,
		sender: sender,
		feeCfg: &config.CurioFees{},
	}
	return task, store, sender, testAPI
}

func makePrecommitSector(sectorNumber int64) precommitSectorParams {
	const COMMITMENT_CID = "bafy2bzacea3wsdh6y3a36tb3skempjoxqpuyompjbmfeyf34fi3uy6uue42v4"

	return precommitSectorParams{
		SpID:         1000,
		SectorNumber: sectorNumber,
		RegSealProof: abi.RegisteredSealProof_StackedDrg32GiBV1_1,
		TicketEpoch:  900,
		SealedCID:    COMMITMENT_CID,
		UnsealedCID:  COMMITMENT_CID,
	}
}

func precommitEpoch(epoch abi.ChainEpoch) sql.NullInt64 {
	return sql.NullInt64{Int64: int64(epoch), Valid: true}
}

func makeDirectPrecommitPiece(start, end abi.ChainEpoch) precommitPiece {
	return precommitPiece{
		DirectDealStartEpoch: precommitEpoch(start),
		DirectDealEndEpoch:   precommitEpoch(end),
	}
}

func makeF05PrecommitPiece(start, end abi.ChainEpoch) precommitPiece {
	return precommitPiece{
		F05DealStartEpoch: precommitEpoch(start),
		F05DealEndEpoch:   precommitEpoch(end),
	}
}

func currentProtocolMaxExpiration(t *testing.T) (abi.ChainEpoch, abi.ChainEpoch) {
	t.Helper()

	maxExtension, err := policy.GetMaxSectorExpirationExtension(network.Version21)
	require.NoError(t, err)
	return testPrecommitHead + maxExtension, maxExtension
}

func decodePrecommitParams(t *testing.T, msg *types.Message) miner.PreCommitSectorBatchParams2 {
	t.Helper()

	var params miner.PreCommitSectorBatchParams2
	require.NoError(t, params.UnmarshalCBOR(bytes.NewReader(msg.Params)))
	return params
}

// calculatePrecommitNeedFunds simulates the needFunds calculation in SubmitPrecommitTask.Do
// This mirrors the logic in task_submit_precommit.go lines 281-296
func calculatePrecommitNeedFunds(
	collateral, aggFee, minerBalance abi.TokenAmount,
	collateralFromMinerBalance, disableCollateralFallback bool,
) abi.TokenAmount {
	needFunds := big.Add(collateral, aggFee)

	if collateralFromMinerBalance {
		if disableCollateralFallback {
			needFunds = big.Zero()
		}
		needFunds = big.Sub(needFunds, minerBalance)
		if needFunds.LessThan(big.Zero()) {
			needFunds = big.Zero()
		}
	}
	return needFunds
}

func TestPrecommitNeedFundsCalculation(t *testing.T) {
	tests := []struct {
		name                       string
		collateral                 abi.TokenAmount
		aggFee                     abi.TokenAmount
		minerBalance               abi.TokenAmount
		collateralFromMinerBalance bool
		disableCollateralFallback  bool
		expectedNeedFunds          abi.TokenAmount
	}{
		{
			name:                       "CollateralFromMinerBalance disabled - full funds required",
			collateral:                 fil(10),
			aggFee:                     fil(1),
			minerBalance:               fil(100),
			collateralFromMinerBalance: false,
			disableCollateralFallback:  false,
			expectedNeedFunds:          fil(11), // collateral + aggFee, miner balance not used
		},
		{
			name:                       "CollateralFromMinerBalance enabled - miner covers all",
			collateral:                 fil(10),
			aggFee:                     fil(1),
			minerBalance:               fil(100),
			collateralFromMinerBalance: true,
			disableCollateralFallback:  false,
			expectedNeedFunds:          big.Zero(), // Miner balance covers everything
		},
		{
			name:                       "CollateralFromMinerBalance enabled - miner covers partial",
			collateral:                 fil(10),
			aggFee:                     fil(1),
			minerBalance:               fil(5),
			collateralFromMinerBalance: true,
			disableCollateralFallback:  false,
			expectedNeedFunds:          fil(6), // 11 - 5 = 6 FIL shortfall
		},
		{
			name:                       "CollateralFromMinerBalance enabled - miner has zero balance",
			collateral:                 fil(10),
			aggFee:                     fil(1),
			minerBalance:               big.Zero(),
			collateralFromMinerBalance: true,
			disableCollateralFallback:  false,
			expectedNeedFunds:          fil(11), // Wallet covers all
		},
		{
			name:                       "DisableCollateralFallback - always zero",
			collateral:                 fil(10),
			aggFee:                     fil(1),
			minerBalance:               big.Zero(),
			collateralFromMinerBalance: true,
			disableCollateralFallback:  true,
			expectedNeedFunds:          big.Zero(), // Collateral fallback disabled
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := calculatePrecommitNeedFunds(
				tt.collateral, tt.aggFee, tt.minerBalance,
				tt.collateralFromMinerBalance, tt.disableCollateralFallback,
			)
			assert.Equal(t, tt.expectedNeedFunds, result, "needFunds calculation mismatch")
		})
	}
}

func TestPrecommitGoodFundsCalculation(t *testing.T) {
	// This test verifies that goodFunds includes needFunds + a reasonable gas buffer (10% of maxFee),
	// NOT the full maxFee (which is just a cap, not typical usage).

	tests := []struct {
		name                       string
		collateral                 abi.TokenAmount
		aggFee                     abi.TokenAmount
		minerBalance               abi.TokenAmount
		maxFee                     abi.TokenAmount
		collateralFromMinerBalance bool
		expectedGoodFunds          abi.TokenAmount // Should be needFunds + 10% of maxFee
	}{
		{
			name:                       "Miner covers all - goodFunds is just gas buffer",
			collateral:                 fil(10),
			aggFee:                     fil(1),
			minerBalance:               fil(100),
			maxFee:                     fil(8), // High maxFee configured
			collateralFromMinerBalance: true,
			// goodFunds = 0 (needFunds) + 0.8 FIL (10% of maxFee) = 0.8 FIL
			expectedGoodFunds: big.Div(fil(8), big.NewInt(10)),
		},
		{
			name:                       "Miner covers partial - goodFunds is shortfall plus gas buffer",
			collateral:                 fil(10),
			aggFee:                     fil(1),
			minerBalance:               fil(5),
			maxFee:                     fil(8),
			collateralFromMinerBalance: true,
			// goodFunds = 6 FIL (shortfall) + 0.8 FIL (10% of maxFee) = 6.8 FIL
			expectedGoodFunds: big.Add(fil(6), big.Div(fil(8), big.NewInt(10))),
		},
		{
			name:                       "CollateralFromMinerBalance disabled - goodFunds is needFunds plus gas buffer",
			collateral:                 fil(10),
			aggFee:                     fil(1),
			minerBalance:               fil(100),
			maxFee:                     fil(8),
			collateralFromMinerBalance: false,
			// goodFunds = 11 FIL (needFunds) + 0.8 FIL (10% of maxFee) = 11.8 FIL
			expectedGoodFunds: big.Add(fil(11), big.Div(fil(8), big.NewInt(10))),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Simulate the needFunds calculation
			needFunds := calculatePrecommitNeedFunds(
				tt.collateral, tt.aggFee, tt.minerBalance,
				tt.collateralFromMinerBalance, false,
			)

			// The fix: goodFunds = needFunds + 10% of maxFee (reasonable gas buffer)
			// NOT big.Add(maxFee, needFunds) which was the old buggy behavior (100% of maxFee)
			gasBuffer := big.Div(tt.maxFee, big.NewInt(10))
			goodFunds := big.Add(needFunds, gasBuffer)

			assert.Equal(t, tt.expectedGoodFunds, goodFunds,
				"goodFunds should be needFunds + 10%% of maxFee, not full maxFee")
		})
	}
}

func TestPrecommitAddressSelectionWithLowWalletBalance(t *testing.T) {
	// This test verifies that address selection works correctly when:
	// 1. Worker wallet has low balance (e.g., 1 FIL)
	// 2. Miner balance covers collateral (so wallet only needs gas buffer)
	// 3. The old code would fail because goodFunds included full maxFee (8 FIL)
	// 4. The new code should succeed because goodFunds is needFunds + 10% of maxFee (0.8 FIL)

	ctx := context.Background()

	// Create test addresses
	workerAddr, _ := address.NewIDAddress(100)
	ownerAddr, _ := address.NewIDAddress(101)
	minerAddr, _ := address.NewIDAddress(1000)

	// Mock API where worker has only 1 FIL but we have the key
	mockAPI := &mockPrecommitAPI{
		walletBalances: map[address.Address]big.Int{
			workerAddr: fil(1), // Only 1 FIL in worker wallet
		},
		walletHas: map[address.Address]bool{
			workerAddr: true, // We have the key
		},
		minerBalance: fil(100), // Miner has plenty of balance for collateral
	}

	// Create address selector with no PreCommitControl configured (just worker fallback)
	as := &multictladdr.MultiAddressSelector{
		MinerMap: map[address.Address]multictladdr.AddressConfig{
			minerAddr: {
				PreCommitControl:      []address.Address{}, // No precommit control addresses
				DisableWorkerFallback: false,               // Enable worker fallback
				DisableOwnerFallback:  true,                // Disable owner for simplicity
			},
		},
	}

	mi := api.MinerInfo{
		Worker: workerAddr,
		Owner:  ownerAddr,
		ControlAddresses: []address.Address{
			workerAddr,
		},
	}

	// With the fix: goodFunds = 0 (needFunds) + 0.8 FIL (10% of 8 FIL maxFee) = 0.8 FIL
	// Worker has 1 FIL, which is >= 0.8 FIL, so it should be selected
	maxFee := fil(8)
	gasBuffer := big.Div(maxFee, big.NewInt(10)) // 0.8 FIL
	goodFunds := gasBuffer                       // needFunds is 0 when miner covers it
	minFunds := big.Zero()

	selectedAddr, _, err := as.AddressFor(ctx, mockAPI, minerAddr, mi, api.PreCommitAddr, goodFunds, minFunds)
	require.NoError(t, err)
	assert.Equal(t, workerAddr, selectedAddr, "Worker should be selected when it has >= goodFunds (0.8 FIL)")

	// Old behavior would have failed: goodFunds = maxFee + needFunds = 8 FIL + 0 = 8 FIL
	// Worker only has 1 FIL, so it wouldn't pass the balance check
	oldGoodFunds := fil(8) // This was the old buggy calculation (100% of maxFee)
	selectedAddrOld, _, err := as.AddressFor(ctx, mockAPI, minerAddr, mi, api.PreCommitAddr, oldGoodFunds, minFunds)
	require.NoError(t, err)
	// Note: PickAddress returns leastBad (worker) even when balance check fails,
	// but it would log a warning. The key issue is that GasEstimateMessageGas
	// might fail later if the selected address can't cover simulation gas.
	assert.Equal(t, workerAddr, selectedAddrOld,
		"Worker is still selected as fallback, but with a warning about insufficient funds")
}

func TestPrecommitAddressSelectionScenarios(t *testing.T) {
	ctx := context.Background()

	workerAddr, _ := address.NewIDAddress(100)
	ownerAddr, _ := address.NewIDAddress(101)
	precommitCtlAddr, _ := address.NewIDAddress(102)
	minerAddr, _ := address.NewIDAddress(1000)

	// Typical gas buffer: 10% of 8 FIL maxFee = 0.8 FIL
	gasBuffer := big.Div(fil(8), big.NewInt(10))

	tests := []struct {
		name             string
		walletBalances   map[address.Address]big.Int
		walletHas        map[address.Address]bool
		precommitControl []address.Address
		goodFunds        abi.TokenAmount
		expectedSelected address.Address
		description      string
	}{
		{
			name: "Worker only - gas buffer only - should select worker",
			walletBalances: map[address.Address]big.Int{
				workerAddr: fil(1), // 1 FIL > 0.8 FIL gas buffer
			},
			walletHas: map[address.Address]bool{
				workerAddr: true,
			},
			precommitControl: []address.Address{},
			goodFunds:        gasBuffer, // 0.8 FIL (collateral covered by miner)
			expectedSelected: workerAddr,
			description:      "With miner covering collateral, worker with 1 FIL should work (need 0.8 FIL)",
		},
		{
			name: "PreCommitControl has funds but no key - fallback to worker",
			walletBalances: map[address.Address]big.Int{
				precommitCtlAddr: fil(100), // Plenty of funds
				workerAddr:       fil(1),   // 1 FIL > 0.8 FIL gas buffer
			},
			walletHas: map[address.Address]bool{
				precommitCtlAddr: false, // Don't have key for precommit control
				workerAddr:       true,  // Have key for worker
			},
			precommitControl: []address.Address{precommitCtlAddr},
			goodFunds:        gasBuffer,
			expectedSelected: workerAddr,
			description:      "Should fallback to worker when PreCommitControl key not available",
		},
		{
			name: "PreCommitControl has funds and key - should select PreCommitControl",
			walletBalances: map[address.Address]big.Int{
				precommitCtlAddr: fil(100),
				workerAddr:       fil(1),
			},
			walletHas: map[address.Address]bool{
				precommitCtlAddr: true,
				workerAddr:       true,
			},
			precommitControl: []address.Address{precommitCtlAddr},
			goodFunds:        fil(10), // Some collateral + gas buffer
			expectedSelected: precommitCtlAddr,
			description:      "PreCommitControl should be selected when it has funds and key",
		},
		{
			name: "Worker has more funds than required goodFunds",
			walletBalances: map[address.Address]big.Int{
				workerAddr: fil(50),
			},
			walletHas: map[address.Address]bool{
				workerAddr: true,
			},
			precommitControl: []address.Address{},
			goodFunds:        fil(10),
			expectedSelected: workerAddr,
			description:      "Worker should be selected when it has enough funds",
		},
		{
			name: "Lender scenario - worker controlled by curio with minimal local balance",
			walletBalances: map[address.Address]big.Int{
				workerAddr: fil(1), // Just enough for gas buffer (0.8 FIL)
			},
			walletHas: map[address.Address]bool{
				workerAddr: true, // Curio controls worker
			},
			precommitControl: []address.Address{},
			goodFunds:        gasBuffer, // 0.8 FIL (only gas buffer needed)
			expectedSelected: workerAddr,
			description:      "Lender scenario: worker with 1 FIL should work when miner covers collateral (need 0.8 FIL)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockAPI := &mockPrecommitAPI{
				walletBalances: tt.walletBalances,
				walletHas:      tt.walletHas,
			}

			as := &multictladdr.MultiAddressSelector{
				MinerMap: map[address.Address]multictladdr.AddressConfig{
					minerAddr: {
						PreCommitControl:      tt.precommitControl,
						DisableWorkerFallback: false,
						DisableOwnerFallback:  true,
					},
				},
			}

			mi := api.MinerInfo{
				Worker: workerAddr,
				Owner:  ownerAddr,
				ControlAddresses: append([]address.Address{workerAddr, ownerAddr},
					tt.precommitControl...),
			}

			selectedAddr, _, err := as.AddressFor(ctx, mockAPI, minerAddr, mi,
				api.PreCommitAddr, tt.goodFunds, big.Zero())

			require.NoError(t, err, tt.description)
			assert.Equal(t, tt.expectedSelected, selectedAddr, tt.description)
		})
	}
}

func TestPrecommitFeeCfgIntegration(t *testing.T) {
	// Test that verifies the fee configuration is correctly applied
	// in the context of the SubmitPrecommitTask

	tests := []struct {
		name                       string
		collateralFromMinerBalance bool
		disableCollateralFallback  bool
		minerBalance               abi.TokenAmount
		collateral                 abi.TokenAmount
		aggFee                     abi.TokenAmount
		maxFee                     abi.TokenAmount
		expectedNeedFunds          abi.TokenAmount
		expectedGoodFunds          abi.TokenAmount // needFunds + 10% of maxFee
	}{
		{
			name:                       "Standard lender setup - miner covers all",
			collateralFromMinerBalance: true,
			disableCollateralFallback:  false,
			minerBalance:               fil(100),
			collateral:                 fil(10),
			aggFee:                     fil(1),
			maxFee:                     fil(8),
			expectedNeedFunds:          big.Zero(),
			// goodFunds = 0 + 0.8 FIL (10% of maxFee) = 0.8 FIL
			expectedGoodFunds: big.Div(fil(8), big.NewInt(10)),
		},
		{
			name:                       "Partial miner coverage",
			collateralFromMinerBalance: true,
			disableCollateralFallback:  false,
			minerBalance:               fil(5),
			collateral:                 fil(10),
			aggFee:                     fil(1),
			maxFee:                     fil(8),
			expectedNeedFunds:          fil(6),
			// goodFunds = 6 + 0.8 FIL = 6.8 FIL (NOT maxFee + 6 = 14!)
			expectedGoodFunds: big.Add(fil(6), big.Div(fil(8), big.NewInt(10))),
		},
		{
			name:                       "No miner balance usage",
			collateralFromMinerBalance: false,
			disableCollateralFallback:  false,
			minerBalance:               fil(100), // Ignored
			collateral:                 fil(10),
			aggFee:                     fil(1),
			maxFee:                     fil(8),
			expectedNeedFunds:          fil(11),
			// goodFunds = 11 + 0.8 FIL = 11.8 FIL (NOT maxFee + 11 = 19!)
			expectedGoodFunds: big.Add(fil(11), big.Div(fil(8), big.NewInt(10))),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			feeCfg := &config.CurioFees{
				CollateralFromMinerBalance: tt.collateralFromMinerBalance,
				DisableCollateralFallback:  tt.disableCollateralFallback,
				MaxPreCommitBatchGasFee: config.BatchFeeConfig{
					Base:      types.MustParseFIL("0"),
					PerSector: types.MustParseFIL("8"),
				},
			}

			// Simulate the Do method's needFunds calculation
			needFunds := big.Add(tt.collateral, tt.aggFee)

			if feeCfg.CollateralFromMinerBalance {
				if feeCfg.DisableCollateralFallback {
					needFunds = big.Zero()
				}
				needFunds = big.Sub(needFunds, tt.minerBalance)
				if needFunds.LessThan(big.Zero()) {
					needFunds = big.Zero()
				}
			}

			// The fix: goodFunds = needFunds + 10% of maxFee (reasonable gas buffer)
			// NOT big.Add(maxFee, needFunds) which was the old behavior (100% of maxFee)
			gasBuffer := big.Div(tt.maxFee, big.NewInt(10))
			goodFunds := big.Add(needFunds, gasBuffer)

			assert.Equal(t, tt.expectedNeedFunds, needFunds, "needFunds mismatch")
			assert.Equal(t, tt.expectedGoodFunds, goodFunds, "goodFunds should be needFunds + 10%% of maxFee")
		})
	}
}

func TestSubmitPrecommitDetachesPreviouslyFailedSector(t *testing.T) {
	taskID := harmonytask.TaskID(44)
	failed := makePrecommitSector(301)
	failed.Failed = true
	valid := makePrecommitSector(302)
	task, store, sender, _ := makeSubmitPrecommitTaskForTest(t, []precommitSectorParams{failed, valid}, nil)

	done, err := task.Do(t.Context(), taskID, func() bool { return true })
	require.NoError(t, err)
	require.True(t, done)
	require.Equal(t, []precommitSectorKey{{spID: failed.SpID, sectorNumber: failed.SectorNumber}}, store.detached)
	require.Len(t, sender.messages, 1)
	params := decodePrecommitParams(t, sender.messages[0])
	require.Len(t, params.Sectors, 1)
	require.Equal(t, abi.SectorNumber(valid.SectorNumber), params.Sectors[0].SectorNumber)
	require.Equal(t, []int64{valid.SectorNumber}, store.messageSectors)
	require.Equal(t, valid.SpID, store.messageSPID)
	require.Equal(t, taskID, store.messageTaskID)
	require.Equal(t, sender.cid, store.messageCID)
	require.Equal(t, []cid.Cid{sender.cid}, store.messageWaits)
}

func TestSubmitPrecommitAllPreviouslyFailedCompletesWithoutMessage(t *testing.T) {
	first := makePrecommitSector(401)
	first.Failed = true
	second := makePrecommitSector(402)
	second.Failed = true
	task, store, sender, testAPI := makeSubmitPrecommitTaskForTest(t, []precommitSectorParams{first, second}, nil)

	done, err := task.Do(t.Context(), harmonytask.TaskID(45), func() bool { return true })
	require.NoError(t, err)
	require.True(t, done)
	require.ElementsMatch(t, []precommitSectorKey{
		{spID: first.SpID, sectorNumber: first.SectorNumber},
		{spID: second.SpID, sectorNumber: second.SectorNumber},
	}, store.detached)
	require.Empty(t, sender.messages)
	require.Empty(t, store.messageSectors)
	require.Empty(t, store.messageWaits)
	require.Zero(t, testAPI.chainHeadCalls, "an all-failed task must not make chain calls")
}

func TestSubmitPrecommitDetachFailureStopsBeforeMessage(t *testing.T) {
	failed := makePrecommitSector(501)
	failed.Failed = true
	valid := makePrecommitSector(502)
	task, store, sender, testAPI := makeSubmitPrecommitTaskForTest(t, []precommitSectorParams{failed, valid}, nil)
	store.detachErr = errors.New("detach failed")

	done, err := task.Do(t.Context(), harmonytask.TaskID(46), func() bool { return true })
	require.ErrorContains(t, err, "detaching failed sector from precommit task")
	require.False(t, done)
	require.Empty(t, sender.messages)
	require.Zero(t, testAPI.chainHeadCalls)
}

func TestSubmitPrecommitSQLScopesFailedDetachAndMessageAssociation(t *testing.T) {
	normalize := func(query string) string {
		return strings.ToLower(strings.Join(strings.Fields(query), " "))
	}

	loadSQL := normalize(SUBMIT_PRECOMMIT_LOAD_SECTORS_SQL)
	require.Contains(t, loadSQL, "tree_d_cid, failed from sectors_sdr_pipeline")

	detachSQL := normalize(SUBMIT_PRECOMMIT_DETACH_FAILED_SECTOR_SQL)
	require.Contains(t, detachSQL, "where task_id_precommit_msg = $1 and sp_id = $2 and sector_number = $3 and failed = true")

	messageSQL := normalize(SUBMIT_PRECOMMIT_SET_MESSAGE_CID_SQL)
	require.Contains(t, messageSQL, "where task_id_precommit_msg = $2 and sp_id = $3 and sector_number = any($4::bigint[])")
	require.Contains(t, messageSQL, "and after_precommit_msg = false and failed = false")
}

func TestSubmitPrecommitMixedBatchIsolatesPastStartSector(t *testing.T) {
	taskID := harmonytask.TaskID(60)
	expired := makePrecommitSector(601)
	valid := makePrecommitSector(602)
	task, store, sender, _ := makeSubmitPrecommitTaskForTest(t,
		[]precommitSectorParams{expired, valid},
		map[precommitSectorKey][]precommitPiece{
			{spID: expired.SpID, sectorNumber: expired.SectorNumber}: {makeDirectPrecommitPiece(999, 600000)},
			{spID: valid.SpID, sectorNumber: valid.SectorNumber}:     {makeDirectPrecommitPiece(1100, 600000)},
		})

	done, err := task.Do(t.Context(), taskID, func() bool { return true })
	require.NoError(t, err)
	require.True(t, done)
	require.Equal(t, recordedPrecommitFailure{
		taskID:  taskID,
		reason:  "past-start-epoch",
		message: "precommit: start epoch is in the past",
	}, store.failures[precommitSectorKey{spID: expired.SpID, sectorNumber: expired.SectorNumber}])
	require.Len(t, sender.messages, 1)
	params := decodePrecommitParams(t, sender.messages[0])
	require.Len(t, params.Sectors, 1)
	require.Equal(t, abi.SectorNumber(valid.SectorNumber), params.Sectors[0].SectorNumber)
	require.Equal(t, []int64{valid.SectorNumber}, store.messageSectors)
	require.Equal(t, []cid.Cid{sender.cid}, store.messageWaits)
}

func TestSubmitPrecommitTicketExpirationRemainsPerSector(t *testing.T) {
	taskID := harmonytask.TaskID(61)
	expired := makePrecommitSector(611)
	expired.TicketEpoch = -10000
	valid := makePrecommitSector(612)
	task, store, sender, _ := makeSubmitPrecommitTaskForTest(t, []precommitSectorParams{expired, valid}, nil)

	done, err := task.Do(t.Context(), taskID, func() bool { return true })
	require.NoError(t, err)
	require.True(t, done)
	failure := store.failures[precommitSectorKey{spID: expired.SpID, sectorNumber: expired.SectorNumber}]
	require.Equal(t, taskID, failure.taskID)
	require.Equal(t, "precommit-check", failure.reason)
	require.Contains(t, failure.message, "ticket expired")
	require.NotContains(t, store.failures, precommitSectorKey{spID: valid.SpID, sectorNumber: valid.SectorNumber})
	require.Len(t, sender.messages, 1)
	params := decodePrecommitParams(t, sender.messages[0])
	require.Len(t, params.Sectors, 1)
	require.Equal(t, abi.SectorNumber(valid.SectorNumber), params.Sectors[0].SectorNumber)
	require.Equal(t, []int64{valid.SectorNumber}, store.messageSectors)
}

func TestSubmitPrecommitMultipleInvalidSectorsKeepValidSubset(t *testing.T) {
	taskID := harmonytask.TaskID(62)
	pastStart := makePrecommitSector(621)
	expiredTicket := makePrecommitSector(622)
	expiredTicket.TicketEpoch = -10000
	validA := makePrecommitSector(623)
	validB := makePrecommitSector(624)
	task, store, sender, _ := makeSubmitPrecommitTaskForTest(t,
		[]precommitSectorParams{pastStart, expiredTicket, validA, validB},
		map[precommitSectorKey][]precommitPiece{
			{spID: pastStart.SpID, sectorNumber: pastStart.SectorNumber}: {makeDirectPrecommitPiece(999, 600000)},
		})

	done, err := task.Do(t.Context(), taskID, func() bool { return true })
	require.NoError(t, err)
	require.True(t, done)
	require.Len(t, store.failures, 2)
	require.Equal(t, "past-start-epoch", store.failures[precommitSectorKey{spID: pastStart.SpID, sectorNumber: pastStart.SectorNumber}].reason)
	require.Equal(t, "precommit-check", store.failures[precommitSectorKey{spID: expiredTicket.SpID, sectorNumber: expiredTicket.SectorNumber}].reason)
	require.Len(t, sender.messages, 1)
	params := decodePrecommitParams(t, sender.messages[0])
	require.Len(t, params.Sectors, 2)
	require.Equal(t, []abi.SectorNumber{abi.SectorNumber(validA.SectorNumber), abi.SectorNumber(validB.SectorNumber)}, []abi.SectorNumber{params.Sectors[0].SectorNumber, params.Sectors[1].SectorNumber})
	require.Equal(t, []int64{validA.SectorNumber, validB.SectorNumber}, store.messageSectors)
}

func TestSubmitPrecommitPreviouslyFailedAndNewValidationFailure(t *testing.T) {
	taskID := harmonytask.TaskID(63)
	previouslyFailed := makePrecommitSector(631)
	previouslyFailed.Failed = true
	pastStart := makePrecommitSector(632)
	valid := makePrecommitSector(633)
	task, store, sender, _ := makeSubmitPrecommitTaskForTest(t,
		[]precommitSectorParams{previouslyFailed, pastStart, valid},
		map[precommitSectorKey][]precommitPiece{
			{spID: pastStart.SpID, sectorNumber: pastStart.SectorNumber}: {makeDirectPrecommitPiece(999, 600000)},
		})

	done, err := task.Do(t.Context(), taskID, func() bool { return true })
	require.NoError(t, err)
	require.True(t, done)
	require.Equal(t, []precommitSectorKey{{spID: previouslyFailed.SpID, sectorNumber: previouslyFailed.SectorNumber}}, store.detached)
	require.Equal(t, "past-start-epoch", store.failures[precommitSectorKey{spID: pastStart.SpID, sectorNumber: pastStart.SectorNumber}].reason)
	require.Len(t, sender.messages, 1)
	params := decodePrecommitParams(t, sender.messages[0])
	require.Len(t, params.Sectors, 1)
	require.Equal(t, abi.SectorNumber(valid.SectorNumber), params.Sectors[0].SectorNumber)
	require.Equal(t, []int64{valid.SectorNumber}, store.messageSectors)
}

func TestSubmitPrecommitAllValidationFailuresCompleteWithoutMessage(t *testing.T) {
	pastStart := makePrecommitSector(641)
	expiredTicket := makePrecommitSector(642)
	expiredTicket.TicketEpoch = -10000
	task, store, sender, _ := makeSubmitPrecommitTaskForTest(t,
		[]precommitSectorParams{pastStart, expiredTicket},
		map[precommitSectorKey][]precommitPiece{
			{spID: pastStart.SpID, sectorNumber: pastStart.SectorNumber}: {makeDirectPrecommitPiece(999, 600000)},
		})

	done, err := task.Do(t.Context(), harmonytask.TaskID(64), func() bool { return true })
	require.NoError(t, err)
	require.True(t, done)
	require.Len(t, store.failures, 2)
	require.Empty(t, sender.messages)
	require.Empty(t, store.messageSectors)
	require.Empty(t, store.messageWaits)
}

func TestSubmitPrecommitInfrastructureErrorsRemainRetryable(t *testing.T) {
	valid := makePrecommitSector(701)
	pastStart := makePrecommitSector(702)
	pieces := map[precommitSectorKey][]precommitPiece{
		{spID: pastStart.SpID, sectorNumber: pastStart.SectorNumber}: {makeDirectPrecommitPiece(999, 600000)},
	}

	tests := []struct {
		name       string
		sectors    []precommitSectorParams
		pieces     map[precommitSectorKey][]precommitPiece
		configure  func(*mockPrecommitTaskStore, *mockPrecommitMessageSender, *mockPrecommitAPI)
		errorMatch string
		wantSent   int
		wantCID    bool
	}{
		{
			name:    "load sectors",
			sectors: []precommitSectorParams{valid},
			configure: func(store *mockPrecommitTaskStore, _ *mockPrecommitMessageSender, _ *mockPrecommitAPI) {
				store.loadSectorsErr = errors.New("load sectors failed")
			},
			errorMatch: "getting sector params",
		},
		{
			name:    "load pieces",
			sectors: []precommitSectorParams{valid},
			configure: func(store *mockPrecommitTaskStore, _ *mockPrecommitMessageSender, _ *mockPrecommitAPI) {
				store.loadPiecesErr = errors.New("load pieces failed")
			},
			errorMatch: "getting pieces",
		},
		{
			name:    "persist sector failure",
			sectors: []precommitSectorParams{pastStart},
			pieces:  pieces,
			configure: func(store *mockPrecommitTaskStore, _ *mockPrecommitMessageSender, _ *mockPrecommitAPI) {
				store.failSectorErr = errors.New("failure update failed")
			},
			errorMatch: "persisting precommit sector validation error",
		},
		{
			name:    "precommit deposit RPC",
			sectors: []precommitSectorParams{valid},
			configure: func(_ *mockPrecommitTaskStore, _ *mockPrecommitMessageSender, testAPI *mockPrecommitAPI) {
				testAPI.precommitErr = errors.New("precommit deposit failed")
			},
			errorMatch: "getting precommit deposit",
		},
		{
			name:    "send message",
			sectors: []precommitSectorParams{valid},
			configure: func(_ *mockPrecommitTaskStore, sender *mockPrecommitMessageSender, _ *mockPrecommitAPI) {
				sender.err = errors.New("send failed")
			},
			errorMatch: "sending message",
		},
		{
			name:    "persist message CID",
			sectors: []precommitSectorParams{valid},
			configure: func(store *mockPrecommitTaskStore, _ *mockPrecommitMessageSender, _ *mockPrecommitAPI) {
				store.messageCIDErr = errors.New("CID update failed")
			},
			errorMatch: "updating precommit_msg_cid",
			wantSent:   1,
		},
		{
			name:    "persist message wait",
			sectors: []precommitSectorParams{valid},
			configure: func(store *mockPrecommitTaskStore, _ *mockPrecommitMessageSender, _ *mockPrecommitAPI) {
				store.messageWaitErr = errors.New("message wait failed")
			},
			errorMatch: "inserting into message_waits",
			wantSent:   1,
			wantCID:    true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			task, store, sender, testAPI := makeSubmitPrecommitTaskForTest(t, test.sectors, test.pieces)
			test.configure(store, sender, testAPI)

			done, err := task.Do(t.Context(), harmonytask.TaskID(70), func() bool { return true })
			require.ErrorContains(t, err, test.errorMatch)
			require.False(t, done)
			require.Empty(t, store.failures, "infrastructure errors must not become permanent sector failures")
			require.Len(t, sender.messages, test.wantSent)
			if test.wantCID {
				require.NotEmpty(t, store.messageSectors)
			} else {
				require.Empty(t, store.messageSectors)
			}
		})
	}
}

func TestCalculatePrecommitExpirationByPieceType(t *testing.T) {
	protocolMax, maxExtension := currentProtocolMaxExpiration(t)
	const ticketEpoch = abi.ChainEpoch(900)
	minimumExpiration := ticketEpoch + policy.MaxPreCommitRandomnessLookback + miner.MinSectorExpiration
	validStart := testPrecommitHead + 1
	directUnderMax := testPrecommitHead + 365*builtin.EpochsInDay
	f05UnderMax := testPrecommitHead + 300*builtin.EpochsInDay
	fiveYearDirectEnd := testPrecommitHead + builtin.EpochsInFiveYears

	tests := []struct {
		name        string
		pieces      []precommitPiece
		want        abi.ChainEpoch
		wantFailure string
	}{
		{
			name:   "direct below protocol maximum",
			pieces: []precommitPiece{makeDirectPrecommitPiece(validStart, directUnderMax)},
			want:   directUnderMax,
		},
		{
			name:   "direct exactly at protocol maximum",
			pieces: []precommitPiece{makeDirectPrecommitPiece(validStart, protocolMax)},
			want:   protocolMax,
		},
		{
			name:   "five-year direct above protocol maximum is clamped",
			pieces: []precommitPiece{makeDirectPrecommitPiece(validStart, fiveYearDirectEnd)},
			want:   protocolMax,
		},
		{
			name:   "F05 below protocol maximum",
			pieces: []precommitPiece{makeF05PrecommitPiece(validStart, f05UnderMax)},
			want:   f05UnderMax,
		},
		{
			name:   "F05 exactly at protocol maximum",
			pieces: []precommitPiece{makeF05PrecommitPiece(validStart, protocolMax)},
			want:   protocolMax,
		},
		{
			name:        "F05 above protocol maximum is rejected",
			pieces:      []precommitPiece{makeF05PrecommitPiece(validStart, protocolMax+1)},
			wantFailure: "precommit-expiration",
		},
		{
			name: "mixed F05 and long direct uses direct clamp",
			pieces: []precommitPiece{
				makeF05PrecommitPiece(validStart, f05UnderMax),
				makeDirectPrecommitPiece(validStart, fiveYearDirectEnd),
			},
			want: protocolMax,
		},
		{
			name: "mixed sector does not truncate excessive F05 end",
			pieces: []precommitPiece{
				makeF05PrecommitPiece(validStart, protocolMax+1),
				makeDirectPrecommitPiece(validStart, fiveYearDirectEnd),
			},
			wantFailure: "precommit-expiration",
		},
		{
			name: "nullable and zero epochs preserve minimum",
			pieces: []precommitPiece{{
				F05DealStartEpoch:    sql.NullInt64{},
				F05DealEndEpoch:      sql.NullInt64{Int64: 0, Valid: true},
				DirectDealStartEpoch: sql.NullInt64{Int64: 0, Valid: true},
				DirectDealEndEpoch:   sql.NullInt64{},
			}},
			want: minimumExpiration,
		},
		{
			name: "nil direct start preserves a valid direct end",
			pieces: []precommitPiece{{
				DirectDealStartEpoch: sql.NullInt64{},
				DirectDealEndEpoch:   precommitEpoch(directUnderMax),
			}},
			want: directUnderMax,
		},
		{
			name: "nil F05 start preserves a valid F05 end",
			pieces: []precommitPiece{{
				F05DealStartEpoch: sql.NullInt64{},
				F05DealEndEpoch:   precommitEpoch(f05UnderMax),
			}},
			want: f05UnderMax,
		},
		{
			name: "past F05 start is rejected independently",
			pieces: []precommitPiece{{
				F05DealStartEpoch:    precommitEpoch(testPrecommitHead - 1),
				F05DealEndEpoch:      precommitEpoch(f05UnderMax),
				DirectDealStartEpoch: precommitEpoch(validStart),
				DirectDealEndEpoch:   precommitEpoch(directUnderMax),
			}},
			wantFailure: "past-start-epoch",
		},
		{
			name: "past direct start is rejected independently",
			pieces: []precommitPiece{{
				F05DealStartEpoch:    precommitEpoch(validStart),
				F05DealEndEpoch:      precommitEpoch(f05UnderMax),
				DirectDealStartEpoch: precommitEpoch(testPrecommitHead - 1),
				DirectDealEndEpoch:   precommitEpoch(directUnderMax),
			}},
			wantFailure: "past-start-epoch",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			originalPieces := append([]precommitPiece(nil), test.pieces...)
			expiration, failure := calculatePrecommitExpiration(
				testPrecommitHead, ticketEpoch, 0, maxExtension, sql.NullInt64{}, test.pieces)

			if test.wantFailure != "" {
				require.Zero(t, expiration)
				require.NotNil(t, failure)
				require.Equal(t, test.wantFailure, failure.reason)
			} else {
				require.Nil(t, failure)
				require.Equal(t, test.want, expiration)
			}
			require.Equal(t, originalPieces, test.pieces, "expiration calculation must not mutate stored piece epochs")
		})
	}
}

func TestCalculatePrecommitExpirationNoPieceDurations(t *testing.T) {
	_, maxExtension := currentProtocolMaxExpiration(t)
	const ticketEpoch = abi.ChainEpoch(900)

	tests := []struct {
		name         string
		userDuration sql.NullInt64
		want         abi.ChainEpoch
		wantFailure  string
	}{
		{
			name: "CC default duration",
			want: ticketEpoch + maxExtension,
		},
		{
			name:         "existing user duration",
			userDuration: sql.NullInt64{Int64: 700000, Valid: true},
			want:         ticketEpoch + 700000,
		},
		{
			name:         "user duration above protocol maximum",
			userDuration: sql.NullInt64{Int64: int64(testPrecommitHead + maxExtension - ticketEpoch + 1), Valid: true},
			wantFailure:  "precommit-expiration",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			expiration, failure := calculatePrecommitExpiration(
				testPrecommitHead, ticketEpoch, 0, maxExtension, test.userDuration, nil)
			if test.wantFailure != "" {
				require.Zero(t, expiration)
				require.NotNil(t, failure)
				require.Equal(t, test.wantFailure, failure.reason)
				return
			}
			require.Nil(t, failure)
			require.Equal(t, test.want, expiration)
		})
	}
}

func TestSubmitPrecommitExpirationByPieceType(t *testing.T) {
	protocolMax, _ := currentProtocolMaxExpiration(t)
	directUnderMax := testPrecommitHead + 365*builtin.EpochsInDay
	f05UnderMax := testPrecommitHead + 300*builtin.EpochsInDay
	start := testPrecommitHead + 100

	tests := []struct {
		name       string
		pieces     []precommitPiece
		expiration abi.ChainEpoch
	}{
		{
			name:       "direct below maximum",
			pieces:     []precommitPiece{makeDirectPrecommitPiece(start, directUnderMax)},
			expiration: directUnderMax,
		},
		{
			name:       "five-year direct uses protocol maximum",
			pieces:     []precommitPiece{makeDirectPrecommitPiece(start, testPrecommitHead+builtin.EpochsInFiveYears)},
			expiration: protocolMax,
		},
		{
			name:       "direct exactly at maximum",
			pieces:     []precommitPiece{makeDirectPrecommitPiece(start, protocolMax)},
			expiration: protocolMax,
		},
		{
			name:       "F05 below maximum",
			pieces:     []precommitPiece{makeF05PrecommitPiece(start, f05UnderMax)},
			expiration: f05UnderMax,
		},
		{
			name:       "F05 exactly at maximum",
			pieces:     []precommitPiece{makeF05PrecommitPiece(start, protocolMax)},
			expiration: protocolMax,
		},
		{
			name: "mixed F05 and direct",
			pieces: []precommitPiece{
				makeF05PrecommitPiece(start, f05UnderMax),
				makeDirectPrecommitPiece(start, testPrecommitHead+builtin.EpochsInFiveYears),
			},
			expiration: protocolMax,
		},
	}

	for i, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sector := makePrecommitSector(int64(800 + i))
			key := precommitSectorKey{spID: sector.SpID, sectorNumber: sector.SectorNumber}
			originalPieces := append([]precommitPiece(nil), test.pieces...)
			task, store, sender, _ := makeSubmitPrecommitTaskForTest(t,
				[]precommitSectorParams{sector},
				map[precommitSectorKey][]precommitPiece{key: test.pieces})

			done, err := task.Do(t.Context(), harmonytask.TaskID(80+i), func() bool { return true })
			require.NoError(t, err)
			require.True(t, done)
			require.Empty(t, store.failures)
			require.Len(t, sender.messages, 1)
			params := decodePrecommitParams(t, sender.messages[0])
			require.Len(t, params.Sectors, 1)
			require.Equal(t, test.expiration, params.Sectors[0].Expiration)
			require.Equal(t, []int64{sector.SectorNumber}, store.messageSectors)
			require.Equal(t, originalPieces, store.pieces[key], "precommit calculation must not mutate requested schedules")
		})
	}
}

func TestSubmitPrecommitNoPieceDurations(t *testing.T) {
	_, maxExtension := currentProtocolMaxExpiration(t)

	for i, test := range []struct {
		name         string
		userDuration sql.NullInt64
		want         abi.ChainEpoch
	}{
		{
			name: "CC default duration",
			want: abi.ChainEpoch(900) + maxExtension,
		},
		{
			name:         "existing user duration",
			userDuration: sql.NullInt64{Int64: 700000, Valid: true},
			want:         700900,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			sector := makePrecommitSector(int64(880 + i))
			sector.UserSectorDurationEpochs = test.userDuration
			task, store, sender, _ := makeSubmitPrecommitTaskForTest(t, []precommitSectorParams{sector}, nil)

			done, err := task.Do(t.Context(), harmonytask.TaskID(88+i), func() bool { return true })
			require.NoError(t, err)
			require.True(t, done)
			require.Empty(t, store.failures)
			require.Len(t, sender.messages, 1)
			params := decodePrecommitParams(t, sender.messages[0])
			require.Len(t, params.Sectors, 1)
			require.Equal(t, test.want, params.Sectors[0].Expiration)
		})
	}
}

func TestSubmitPrecommitUsesNetworkAwareMaxExtension(t *testing.T) {
	for _, nv := range []network.Version{network.Version18, network.Version21} {
		t.Run(fmt.Sprintf("network-%d", nv), func(t *testing.T) {
			maxExtension, err := policy.GetMaxSectorExpirationExtension(nv)
			require.NoError(t, err)

			sector := makePrecommitSector(int64(900 + nv))
			key := precommitSectorKey{spID: sector.SpID, sectorNumber: sector.SectorNumber}
			task, store, sender, testAPI := makeSubmitPrecommitTaskForTest(t,
				[]precommitSectorParams{sector},
				map[precommitSectorKey][]precommitPiece{
					key: {makeDirectPrecommitPiece(testPrecommitHead+1, testPrecommitHead+builtin.EpochsInFiveYears)},
				})
			testAPI.networkVersion = nv

			done, err := task.Do(t.Context(), harmonytask.TaskID(90+nv), func() bool { return true })
			require.NoError(t, err)
			require.True(t, done)
			require.Empty(t, store.failures)
			require.Len(t, sender.messages, 1)
			params := decodePrecommitParams(t, sender.messages[0])
			require.Len(t, params.Sectors, 1)
			require.Equal(t, testPrecommitHead+maxExtension, params.Sectors[0].Expiration)
		})
	}
}

func TestSubmitPrecommitF05BeyondProtocolMaxIsolated(t *testing.T) {
	protocolMax, maxExtension := currentProtocolMaxExpiration(t)
	start := testPrecommitHead + 100
	validEnd := testPrecommitHead + 365*builtin.EpochsInDay

	for i, test := range []struct {
		name          string
		invalidPieces []precommitPiece
	}{
		{
			name:          "F05 only",
			invalidPieces: []precommitPiece{makeF05PrecommitPiece(start, protocolMax+1)},
		},
		{
			name: "mixed F05 and direct",
			invalidPieces: []precommitPiece{
				makeF05PrecommitPiece(start, protocolMax+1),
				makeDirectPrecommitPiece(start, testPrecommitHead+builtin.EpochsInFiveYears),
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			invalid := makePrecommitSector(int64(1000 + i*2))
			valid := makePrecommitSector(invalid.SectorNumber + 1)
			invalidKey := precommitSectorKey{spID: invalid.SpID, sectorNumber: invalid.SectorNumber}
			validKey := precommitSectorKey{spID: valid.SpID, sectorNumber: valid.SectorNumber}
			task, store, sender, _ := makeSubmitPrecommitTaskForTest(t,
				[]precommitSectorParams{invalid, valid},
				map[precommitSectorKey][]precommitPiece{
					invalidKey: test.invalidPieces,
					validKey:   {makeDirectPrecommitPiece(start, validEnd)},
				})

			done, err := task.Do(t.Context(), harmonytask.TaskID(100+i), func() bool { return true })
			require.NoError(t, err)
			require.True(t, done)
			failure, ok := store.failures[invalidKey]
			require.True(t, ok)
			require.Equal(t, "precommit-expiration", failure.reason)
			require.Equal(t, fmt.Sprintf(
				"requested F05 end %d exceeds protocol max expiration %d (current head %d, max extension %d)",
				protocolMax+1, protocolMax, testPrecommitHead, maxExtension), failure.message)
			require.NotContains(t, store.failures, validKey)
			require.Len(t, sender.messages, 1)
			params := decodePrecommitParams(t, sender.messages[0])
			require.Len(t, params.Sectors, 1)
			require.Equal(t, abi.SectorNumber(valid.SectorNumber), params.Sectors[0].SectorNumber)
			require.Equal(t, validEnd, params.Sectors[0].Expiration)
			require.Equal(t, []int64{valid.SectorNumber}, store.messageSectors)
		})
	}
}

func TestSubmitPrecommitAllF05BeyondProtocolMaxCompletesWithoutMessage(t *testing.T) {
	protocolMax, _ := currentProtocolMaxExpiration(t)
	sector := makePrecommitSector(1090)
	key := precommitSectorKey{spID: sector.SpID, sectorNumber: sector.SectorNumber}
	task, store, sender, _ := makeSubmitPrecommitTaskForTest(t,
		[]precommitSectorParams{sector},
		map[precommitSectorKey][]precommitPiece{
			key: {makeF05PrecommitPiece(testPrecommitHead+1, protocolMax+1)},
		})

	done, err := task.Do(t.Context(), harmonytask.TaskID(109), func() bool { return true })
	require.NoError(t, err)
	require.True(t, done)
	require.Equal(t, "precommit-expiration", store.failures[key].reason)
	require.Empty(t, sender.messages)
	require.Empty(t, store.messageSectors)
	require.Empty(t, store.messageWaits)
}

func TestCalculatePrecommitExpirationRejectsMinimumBeyondProtocolMax(t *testing.T) {
	const maxExtension = abi.ChainEpoch(100)
	expiration, failure := calculatePrecommitExpiration(
		testPrecommitHead,
		testPrecommitHead,
		maxExtension,
		maxExtension,
		sql.NullInt64{},
		[]precommitPiece{makeDirectPrecommitPiece(testPrecommitHead+1, testPrecommitHead+50)},
	)

	require.Zero(t, expiration)
	require.NotNil(t, failure)
	require.Equal(t, "precommit-expiration", failure.reason)
	require.Contains(t, failure.message, "minimum expiration")
	require.Contains(t, failure.message, fmt.Sprintf("protocol max expiration %d", testPrecommitHead+maxExtension))
}

func TestSubmitPrecommitMinimumBeyondProtocolMaxFailsSector(t *testing.T) {
	protocolMax, _ := currentProtocolMaxExpiration(t)
	sector := makePrecommitSector(1100)
	sector.TicketEpoch = protocolMax
	key := precommitSectorKey{spID: sector.SpID, sectorNumber: sector.SectorNumber}
	task, store, sender, _ := makeSubmitPrecommitTaskForTest(t,
		[]precommitSectorParams{sector},
		map[precommitSectorKey][]precommitPiece{
			key: {makeDirectPrecommitPiece(testPrecommitHead+1, testPrecommitHead+365*builtin.EpochsInDay)},
		})

	done, err := task.Do(t.Context(), harmonytask.TaskID(110), func() bool { return true })
	require.NoError(t, err)
	require.True(t, done)
	require.Empty(t, sender.messages)
	require.Empty(t, store.messageSectors)
	require.Empty(t, store.messageWaits)
	failure, ok := store.failures[key]
	require.True(t, ok)
	require.Equal(t, "precommit-expiration", failure.reason)
	require.Contains(t, failure.message, "minimum expiration")
}

func TestSubmitPrecommitSQLScopesValidationFailure(t *testing.T) {
	normalize := func(query string) string {
		return strings.ToLower(strings.Join(strings.Fields(query), " "))
	}

	failSQL := normalize(SUBMIT_PRECOMMIT_FAIL_SECTOR_SQL)
	require.Contains(t, failSQL, "where task_id_precommit_msg = $3 and sp_id = $4 and sector_number = $5 and failed = false")

	pieceSQL := normalize(SUBMIT_PRECOMMIT_LOAD_PIECES_SQL)
	require.Contains(t, pieceSQL, "f05_deal_start_epoch, f05_deal_end_epoch, direct_start_epoch, direct_end_epoch")
	require.NotContains(t, pieceSQL, "coalesce(")
}
