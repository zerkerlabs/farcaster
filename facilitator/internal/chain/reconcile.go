package chain

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	ethcrypto "github.com/ethereum/go-ethereum/crypto"

	"github.com/zerkerlabs/farcaster/x402types"
)

// Reconcile errors — the outcome of re-checking the chain for an authorization
// whose settlement row is stuck in_flight (spec 0007 follow-up #179: a
// post-submit record-write failure, or a transient submit/inclusion-timeout
// error T5 leaves in_flight with no store "release"). Distinct from the
// Submit outcome errors above: Reconcile never broadcasts, it only reads.
var (
	// ErrAuthorizationNotSettled means the authorization's nonce was never
	// consumed on-chain and its validity window (validBefore) has elapsed, so
	// it can never settle now — a true terminal failure, not merely "not yet".
	ErrAuthorizationNotSettled = errors.New("chain: authorization did not settle on-chain and its validity window has elapsed")

	// ErrReconcileIndeterminate means the authorization's nonce is not yet
	// consumed but its validity window has not elapsed either — it may still
	// land later (or the row may simply not be stale enough to check yet).
	// The caller must leave the row in_flight rather than guess.
	ErrReconcileIndeterminate = errors.New("chain: authorization outcome cannot yet be determined")
)

// authorizationStateSelector is the 4-byte selector for the EIP-3009 view
// call authorizationState(address,bytes32) — every EIP-3009 token (including
// USDC) exposes this to report whether an authorizer's nonce has been used.
var authorizationStateSelector = ethcrypto.Keccak256([]byte("authorizationState(address,bytes32)"))[:4]

// authorizationUsedEventSig is keccak256("AuthorizationUsed(address,bytes32)"),
// the EIP-3009 reference event (indexed on authorizer, nonce) a successful
// transferWithAuthorization emits — how Reconcile locates the settling
// transaction's hash when a nonce is found consumed.
var authorizationUsedEventSig = ethcrypto.Keccak256Hash([]byte("AuthorizationUsed(address,bytes32)"))

// Reconcile re-checks the chain for authz's true outcome, for a settlement row
// stuck in_flight past its staleness threshold: it never re-broadcasts, only
// reads. It returns the real transaction hash and a nil error when the
// authorization's nonce has been consumed on-chain (the caller should mark the
// row settled with this hash); [ErrAuthorizationNotSettled] when the nonce is
// unused and the authorization's validity window has elapsed (the caller
// should mark the row failed); or [ErrReconcileIndeterminate] when neither is
// true yet (the caller must leave the row in_flight).
func (c *Client) Reconcile(ctx context.Context, authz x402types.Authorization, now time.Time) (common.Hash, error) {
	used, err := c.authorizationUsed(ctx, authz)
	if err != nil {
		return common.Hash{}, err
	}
	if !used {
		validBefore, ok := new(big.Int).SetString(authz.ValidBefore, 10)
		if !ok {
			return common.Hash{}, ErrMalformedAuthorization
		}
		if now.Unix() < validBefore.Int64() {
			return common.Hash{}, ErrReconcileIndeterminate
		}
		return common.Hash{}, ErrAuthorizationNotSettled
	}
	return c.findAuthorizationUsedTx(ctx, authz)
}

// authorizationUsed calls the USDC contract's authorizationState(address,bytes32)
// view function for authz's (from, nonce) pair.
func (c *Client) authorizationUsed(ctx context.Context, authz x402types.Authorization) (bool, error) {
	from, err := addressWord(authz.From)
	if err != nil {
		return false, err
	}
	nonce, err := bytes32Word(authz.Nonce)
	if err != nil {
		return false, err
	}

	calldata := make([]byte, 0, len(authorizationStateSelector)+64)
	calldata = append(calldata, authorizationStateSelector...)
	calldata = append(calldata, from...)
	calldata = append(calldata, nonce...)

	out, err := c.eth.CallContract(ctx, ethereum.CallMsg{To: &c.usdc, Data: calldata}, nil)
	if err != nil {
		return false, fmt.Errorf("chain: call authorizationState: %w", err)
	}
	return decodeBoolWord(out)
}

// findAuthorizationUsedTx locates the AuthorizationUsed(authz.From, authz.Nonce)
// log within the client's reconcile lookback window and returns its
// transaction hash. Both topics are indexed, so this is a bloom-filtered scan,
// not a full block-body read.
func (c *Client) findAuthorizationUsedTx(ctx context.Context, authz x402types.Authorization) (common.Hash, error) {
	nonceBytes, err := decodeHexFixed(authz.Nonce, 32)
	if err != nil {
		return common.Hash{}, err
	}
	if !common.IsHexAddress(authz.From) {
		return common.Hash{}, ErrMalformedAuthorization
	}
	authorizerTopic := common.BytesToHash(common.HexToAddress(authz.From).Bytes())
	nonceTopic := common.BytesToHash(nonceBytes)

	logs, err := c.eth.FilterLogs(ctx, ethereum.FilterQuery{
		FromBlock: c.reconcileFromBlock(ctx),
		Addresses: []common.Address{c.usdc},
		Topics:    [][]common.Hash{{authorizationUsedEventSig}, {authorizerTopic}, {nonceTopic}},
	})
	if err != nil {
		return common.Hash{}, fmt.Errorf("chain: filter AuthorizationUsed logs: %w", err)
	}
	if len(logs) == 0 {
		return common.Hash{}, fmt.Errorf("chain: authorization consumed on-chain but no AuthorizationUsed log found within the lookback window")
	}

	latest := logs[0]
	for _, l := range logs[1:] {
		if l.BlockNumber > latest.BlockNumber {
			latest = l
		}
	}
	return latest.TxHash, nil
}

// reconcileFromBlock returns the lower bound of the log-filter window: the
// current head minus the client's reconcile lookback, floored at the genesis
// block. A head fetch failure widens the search to the full chain (nil,
// meaning "from genesis") rather than failing the whole reconciliation.
func (c *Client) reconcileFromBlock(ctx context.Context) *big.Int {
	head, err := c.eth.HeaderByNumber(ctx, nil)
	if err != nil || head.Number == nil {
		return nil
	}
	start := new(big.Int).Sub(head.Number, new(big.Int).SetUint64(c.reconcileLookback))
	if start.Sign() < 0 {
		return big.NewInt(0)
	}
	return start
}

// decodeBoolWord decodes a single ABI-encoded bool return word: 32 bytes,
// zero except (optionally) the last, which is 0 or 1.
func decodeBoolWord(b []byte) (bool, error) {
	if len(b) != 32 {
		return false, fmt.Errorf("chain: unexpected authorizationState result length %d", len(b))
	}
	for _, x := range b[:31] {
		if x != 0 {
			return false, errors.New("chain: malformed authorizationState result")
		}
	}
	return b[31] != 0, nil
}
