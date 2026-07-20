package chain

import (
	"context"
	"errors"
	"math/big"
	"sync"
	"testing"
	"time"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	ethcrypto "github.com/ethereum/go-ethereum/crypto"
)

var testChainID = big.NewInt(84532) // Base Sepolia

// mockEth is a controllable EthClient: sane defaults for the read calls, with
// per-test overrides for errors and the receipt sequence. No live network.
type mockEth struct {
	mu sync.Mutex

	pendingNonce  uint64
	nonceCalls    int
	nonceErr      error
	tipErr        error
	baseFee       *big.Int
	headErr       error
	gas           uint64
	gasErr        error
	sendErr       error
	sent          []*types.Transaction
	receiptFn     func(common.Hash) (*types.Receipt, error)
	defaultStatus uint64
	balance       *big.Int
	balanceErr    error

	callContractFn func(ethereum.CallMsg) ([]byte, error)
	filterLogsFn   func(ethereum.FilterQuery) ([]types.Log, error)
	headNumber     *big.Int
}

func newMockEth() *mockEth {
	return &mockEth{
		pendingNonce:  7,
		baseFee:       big.NewInt(1_000_000_000),
		gas:           70_000,
		defaultStatus: types.ReceiptStatusSuccessful,
	}
}

func (m *mockEth) PendingNonceAt(context.Context, common.Address) (uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nonceCalls++
	return m.pendingNonce, m.nonceErr
}

func (m *mockEth) SuggestGasTipCap(context.Context) (*big.Int, error) {
	return big.NewInt(1_500_000_000), m.tipErr
}

func (m *mockEth) HeaderByNumber(context.Context, *big.Int) (*types.Header, error) {
	if m.headErr != nil {
		return nil, m.headErr
	}
	return &types.Header{BaseFee: m.baseFee, Number: m.headNumber}, nil
}

func (m *mockEth) EstimateGas(context.Context, ethereum.CallMsg) (uint64, error) {
	return m.gas, m.gasErr
}

func (m *mockEth) SendTransaction(_ context.Context, tx *types.Transaction) error {
	if m.sendErr != nil {
		return m.sendErr
	}
	m.mu.Lock()
	m.sent = append(m.sent, tx)
	m.mu.Unlock()
	return nil
}

func (m *mockEth) TransactionReceipt(_ context.Context, h common.Hash) (*types.Receipt, error) {
	if m.receiptFn != nil {
		return m.receiptFn(h)
	}
	return &types.Receipt{Status: m.defaultStatus, TxHash: h}, nil
}

func (m *mockEth) BalanceAt(context.Context, common.Address, *big.Int) (*big.Int, error) {
	if m.balanceErr != nil {
		return nil, m.balanceErr
	}
	if m.balance != nil {
		return m.balance, nil
	}
	return big.NewInt(0), nil
}

func (m *mockEth) CallContract(_ context.Context, call ethereum.CallMsg, _ *big.Int) ([]byte, error) {
	if m.callContractFn != nil {
		return m.callContractFn(call)
	}
	return make([]byte, 32), nil // authorizationState defaults to "not used"
}

func (m *mockEth) FilterLogs(_ context.Context, query ethereum.FilterQuery) ([]types.Log, error) {
	if m.filterLogsFn != nil {
		return m.filterLogsFn(query)
	}
	return nil, nil
}

func (m *mockEth) sentCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sent)
}

func newTestClient(t *testing.T, eth EthClient) (*Client, *LocalSigner) {
	t.Helper()
	key, _ := ethcrypto.GenerateKey()
	signer := NewLocalSigner(key)
	c := NewClient(eth, signer, testChainID, Config{
		USDC:             common.HexToAddress("0x036CbD53842c5426634e7929541eC2318f3dCF7e"),
		InclusionTimeout: 200 * time.Millisecond,
		PollInterval:     5 * time.Millisecond,
	})
	return c, signer
}

func TestSubmit_Success(t *testing.T) {
	eth := newMockEth()
	c, signer := newTestClient(t, eth)

	hash, err := c.Submit(context.Background(), sampleAuthorization(), sampleSignature(1))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if (hash == common.Hash{}) {
		t.Fatal("expected non-zero tx hash")
	}
	if eth.sentCount() != 1 {
		t.Fatalf("sent %d transactions, want 1", eth.sentCount())
	}

	tx := eth.sent[0]
	if tx.Type() != types.DynamicFeeTxType {
		t.Fatalf("tx type = %d, want dynamic fee (%d)", tx.Type(), types.DynamicFeeTxType)
	}
	if tx.Nonce() != 7 {
		t.Fatalf("nonce = %d, want 7", tx.Nonce())
	}
	if *tx.To() != c.usdc {
		t.Fatalf("to = %s, want USDC %s", tx.To(), c.usdc)
	}
	if tx.Value().Sign() != 0 {
		t.Fatalf("value = %s, want 0 (token move is in the calldata)", tx.Value())
	}
	// feeCap must be 2*baseFee + tip = 2e9 + 1.5e9.
	if want := big.NewInt(3_500_000_000); tx.GasFeeCap().Cmp(want) != 0 {
		t.Fatalf("gasFeeCap = %s, want %s", tx.GasFeeCap(), want)
	}
	// The transaction is actually signed by the gas key.
	sender, err := types.Sender(types.LatestSignerForChainID(testChainID), tx)
	if err != nil {
		t.Fatalf("recover sender: %v", err)
	}
	if sender != signer.Address() {
		t.Fatalf("sender = %s, want gas wallet %s", sender, signer.Address())
	}
}

func TestSubmit_Reverted(t *testing.T) {
	eth := newMockEth()
	eth.defaultStatus = types.ReceiptStatusFailed
	c, _ := newTestClient(t, eth)

	hash, err := c.Submit(context.Background(), sampleAuthorization(), sampleSignature(1))
	if !errors.Is(err, ErrReverted) {
		t.Fatalf("err = %v, want ErrReverted", err)
	}
	// The hash is still returned so the caller can record the reverted tx.
	if (hash == common.Hash{}) {
		t.Fatal("expected tx hash even on revert")
	}
}

func TestSubmit_InclusionTimeout(t *testing.T) {
	eth := newMockEth()
	eth.receiptFn = func(common.Hash) (*types.Receipt, error) {
		return nil, ethereum.NotFound // never mined
	}
	c, _ := newTestClient(t, eth)

	hash, err := c.Submit(context.Background(), sampleAuthorization(), sampleSignature(1))
	if !errors.Is(err, ErrInclusionTimeout) {
		t.Fatalf("err = %v, want ErrInclusionTimeout", err)
	}
	if (hash == common.Hash{}) {
		t.Fatal("expected tx hash on inclusion timeout (tx may still land)")
	}
}

func TestSubmit_GasEstimationFailure(t *testing.T) {
	eth := newMockEth()
	eth.gasErr = errors.New("execution reverted: insufficient balance")
	c, _ := newTestClient(t, eth)

	_, err := c.Submit(context.Background(), sampleAuthorization(), sampleSignature(1))
	if !errors.Is(err, ErrGasEstimation) {
		t.Fatalf("err = %v, want ErrGasEstimation", err)
	}
	if eth.sentCount() != 0 {
		t.Fatal("must not broadcast when gas estimation fails")
	}
}

func TestSubmit_MalformedAuthorizationNeverBroadcasts(t *testing.T) {
	eth := newMockEth()
	c, _ := newTestClient(t, eth)

	authz := sampleAuthorization()
	authz.From = "0xnothex"
	_, err := c.Submit(context.Background(), authz, sampleSignature(1))
	if !errors.Is(err, ErrMalformedAuthorization) {
		t.Fatalf("err = %v, want ErrMalformedAuthorization", err)
	}
	if eth.sentCount() != 0 {
		t.Fatal("must not broadcast a malformed authorization")
	}
}

// Sequential submits take consecutive gas-wallet nonces from a single fetch.
func TestSubmit_SequentialNonces(t *testing.T) {
	eth := newMockEth()
	c, _ := newTestClient(t, eth)

	for i := 0; i < 3; i++ {
		if _, err := c.Submit(context.Background(), sampleAuthorization(), sampleSignature(1)); err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
	}
	if eth.nonceCalls != 1 {
		t.Fatalf("PendingNonceAt called %d times, want 1 (cached after first)", eth.nonceCalls)
	}
	for i, tx := range eth.sent {
		if want := uint64(7 + i); tx.Nonce() != want {
			t.Fatalf("tx %d nonce = %d, want %d", i, tx.Nonce(), want)
		}
	}
}

// A failed broadcast must not consume the nonce: the next submit resyncs it
// from the node rather than leaving a gap.
func TestSubmit_NonceResyncsAfterSendFailure(t *testing.T) {
	eth := newMockEth()
	eth.sendErr = errors.New("rpc: connection reset")
	c, _ := newTestClient(t, eth)

	if _, err := c.Submit(context.Background(), sampleAuthorization(), sampleSignature(1)); err == nil {
		t.Fatal("expected broadcast error")
	}

	// The node has since accepted the earlier pending tx; nonce advanced to 8.
	eth.sendErr = nil
	eth.pendingNonce = 8
	if _, err := c.Submit(context.Background(), sampleAuthorization(), sampleSignature(1)); err != nil {
		t.Fatalf("second submit: %v", err)
	}
	if eth.nonceCalls != 2 {
		t.Fatalf("PendingNonceAt called %d times, want 2 (resync after failure)", eth.nonceCalls)
	}
	if got := eth.sent[0].Nonce(); got != 8 {
		t.Fatalf("resynced nonce = %d, want 8", got)
	}
}

func TestSubmit_NoBaseFee(t *testing.T) {
	eth := newMockEth()
	eth.baseFee = nil
	c, _ := newTestClient(t, eth)

	if _, err := c.Submit(context.Background(), sampleAuthorization(), sampleSignature(1)); !errors.Is(err, ErrNoBaseFee) {
		t.Fatalf("err = %v, want ErrNoBaseFee", err)
	}
}
