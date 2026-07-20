package chain

import (
	"context"
	"errors"
	"math/big"
	"testing"
	"time"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// authorizationStateResult builds the 32-byte ABI-encoded bool result
// authorizationState(address,bytes32) returns.
func authorizationStateResult(used bool) []byte {
	out := make([]byte, 32)
	if used {
		out[31] = 1
	}
	return out
}

func TestReconcile_SettledOnChainReturnsRealTxHash(t *testing.T) {
	authz := sampleAuthorization()
	wantHash := common.HexToHash("0xdeadbeef")
	authorizerTopic := common.BytesToHash(common.HexToAddress(authz.From).Bytes())
	nonceBytes, _ := decodeHexFixed(authz.Nonce, 32)
	nonceTopic := common.BytesToHash(nonceBytes)

	eth := newMockEth()
	eth.headNumber = big.NewInt(1_000_000)
	eth.callContractFn = func(ethereum.CallMsg) ([]byte, error) {
		return authorizationStateResult(true), nil
	}
	eth.filterLogsFn = func(q ethereum.FilterQuery) ([]types.Log, error) {
		if len(q.Topics) != 3 || q.Topics[0][0] != authorizationUsedEventSig ||
			q.Topics[1][0] != authorizerTopic || q.Topics[2][0] != nonceTopic {
			t.Fatalf("unexpected filter query topics: %+v", q.Topics)
		}
		return []types.Log{{TxHash: wantHash, BlockNumber: 999_990}}, nil
	}
	c, _ := newTestClient(t, eth)

	hash, err := c.Reconcile(context.Background(), authz, time.Unix(1_700_000_000, 0))
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if hash != wantHash {
		t.Fatalf("hash = %s, want %s", hash, wantHash)
	}
}

// When more than one matching log is somehow returned, Reconcile picks the
// highest block number rather than an arbitrary one.
func TestReconcile_SettledPicksLatestLogOnMultipleMatches(t *testing.T) {
	authz := sampleAuthorization()
	older := common.HexToHash("0x01")
	newer := common.HexToHash("0x02")

	eth := newMockEth()
	eth.headNumber = big.NewInt(1_000_000)
	eth.callContractFn = func(ethereum.CallMsg) ([]byte, error) {
		return authorizationStateResult(true), nil
	}
	eth.filterLogsFn = func(ethereum.FilterQuery) ([]types.Log, error) {
		return []types.Log{
			{TxHash: older, BlockNumber: 100},
			{TxHash: newer, BlockNumber: 200},
		}, nil
	}
	c, _ := newTestClient(t, eth)

	hash, err := c.Reconcile(context.Background(), authz, time.Unix(1_700_000_000, 0))
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if hash != newer {
		t.Fatalf("hash = %s, want the latest log's hash %s", hash, newer)
	}
}

func TestReconcile_NeverLandedPastValidBeforeIsTerminal(t *testing.T) {
	authz := sampleAuthorization() // ValidBefore = 1793000000
	eth := newMockEth()
	eth.headNumber = big.NewInt(1_000_000)
	eth.callContractFn = func(ethereum.CallMsg) ([]byte, error) {
		return authorizationStateResult(false), nil
	}
	c, _ := newTestClient(t, eth)

	// now is well past ValidBefore.
	_, err := c.Reconcile(context.Background(), authz, time.Unix(1_800_000_000, 0))
	if !errors.Is(err, ErrAuthorizationNotSettled) {
		t.Fatalf("err = %v, want ErrAuthorizationNotSettled", err)
	}
}

func TestReconcile_UnusedWithinValidityWindowIsIndeterminate(t *testing.T) {
	authz := sampleAuthorization() // ValidBefore = 1793000000
	eth := newMockEth()
	eth.headNumber = big.NewInt(1_000_000)
	eth.callContractFn = func(ethereum.CallMsg) ([]byte, error) {
		return authorizationStateResult(false), nil
	}
	c, _ := newTestClient(t, eth)

	// now is still before ValidBefore: the authorization could still land.
	_, err := c.Reconcile(context.Background(), authz, time.Unix(1_700_000_000, 0))
	if !errors.Is(err, ErrReconcileIndeterminate) {
		t.Fatalf("err = %v, want ErrReconcileIndeterminate", err)
	}
}

func TestReconcile_AuthorizationStateCallFails(t *testing.T) {
	authz := sampleAuthorization()
	eth := newMockEth()
	eth.callContractFn = func(ethereum.CallMsg) ([]byte, error) {
		return nil, errors.New("rpc: connection reset")
	}
	c, _ := newTestClient(t, eth)

	_, err := c.Reconcile(context.Background(), authz, time.Unix(1_700_000_000, 0))
	if err == nil {
		t.Fatal("expected an error when the authorizationState call fails")
	}
	if errors.Is(err, ErrAuthorizationNotSettled) || errors.Is(err, ErrReconcileIndeterminate) {
		t.Fatalf("err = %v, want a plain RPC-failure error, not a decided outcome", err)
	}
}

func TestReconcile_SettledButLogNotFoundIsAnError(t *testing.T) {
	authz := sampleAuthorization()
	eth := newMockEth()
	eth.headNumber = big.NewInt(1_000_000)
	eth.callContractFn = func(ethereum.CallMsg) ([]byte, error) {
		return authorizationStateResult(true), nil
	}
	eth.filterLogsFn = func(ethereum.FilterQuery) ([]types.Log, error) {
		return nil, nil // consumed, but no matching log within the lookback window
	}
	c, _ := newTestClient(t, eth)

	_, err := c.Reconcile(context.Background(), authz, time.Unix(1_700_000_000, 0))
	if err == nil {
		t.Fatal("expected an error when no AuthorizationUsed log is found for a consumed nonce")
	}
}

func TestDecodeBoolWord(t *testing.T) {
	if used, err := decodeBoolWord(authorizationStateResult(true)); err != nil || !used {
		t.Fatalf("decodeBoolWord(true) = %v, %v", used, err)
	}
	if used, err := decodeBoolWord(authorizationStateResult(false)); err != nil || used {
		t.Fatalf("decodeBoolWord(false) = %v, %v", used, err)
	}
	if _, err := decodeBoolWord([]byte{1, 2, 3}); err == nil {
		t.Fatal("expected an error decoding a wrong-length word")
	}
	malformed := make([]byte, 32)
	malformed[0] = 1
	if _, err := decodeBoolWord(malformed); err == nil {
		t.Fatal("expected an error decoding a non-canonical bool word")
	}
}
