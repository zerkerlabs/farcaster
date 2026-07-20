package chain

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

// Readiness errors surfaced by /healthz (spec 0007 T7). Unlike the submission
// outcome errors above, these are protocol-level: an unreachable RPC or an
// under-funded gas wallet mean /settle must fail closed (503) rather than
// hang against a dead node or silently revert for lack of gas.
var (
	// ErrRPCUnreachable means the chain RPC endpoint did not answer a
	// liveness read (fetching the chain head or the gas wallet's balance).
	ErrRPCUnreachable = errors.New("chain: rpc unreachable")

	// ErrGasBalanceLow means the gas wallet's balance is below the
	// configured low-water mark.
	ErrGasBalanceLow = errors.New("chain: gas wallet balance below low-water mark")
)

// CheckReady reports whether eth is reachable and gasWallet's balance is at
// least lowWaterMark. It is the /healthz readiness check (spec 0007 T7).
func CheckReady(ctx context.Context, eth EthClient, gasWallet common.Address, lowWaterMark *big.Int) error {
	if _, err := eth.HeaderByNumber(ctx, nil); err != nil {
		return ErrRPCUnreachable
	}
	balance, err := eth.BalanceAt(ctx, gasWallet, nil)
	if err != nil {
		return ErrRPCUnreachable
	}
	if balance.Cmp(lowWaterMark) < 0 {
		return ErrGasBalanceLow
	}
	return nil
}

// ReadinessChecker is a minimal, Signer-less readiness probe over a chain RPC
// connection: whether the node is reachable and a configured gas wallet's
// balance is at least a low-water mark. It deliberately does not require a
// [Signer] — /healthz only needs to read the wallet's balance, not sign
// anything — so readiness can be wired ahead of the settle path's signer
// selection (T5).
type ReadinessChecker struct {
	eth          EthClient
	gasWallet    common.Address
	lowWaterMark *big.Int
}

// NewReadinessChecker builds a ReadinessChecker over the given node.
func NewReadinessChecker(eth EthClient, gasWallet common.Address, lowWaterMark *big.Int) *ReadinessChecker {
	return &ReadinessChecker{eth: eth, gasWallet: gasWallet, lowWaterMark: lowWaterMark}
}

// DialReadinessChecker connects to the RPC endpoint and returns a
// ReadinessChecker over it — the production constructor, mirroring [Dial].
func DialReadinessChecker(ctx context.Context, rpcURL string, gasWallet common.Address, lowWaterMark *big.Int) (*ReadinessChecker, error) {
	ec, err := ethclient.DialContext(ctx, rpcURL)
	if err != nil {
		return nil, fmt.Errorf("chain: dial rpc: %w", err)
	}
	return NewReadinessChecker(ec, gasWallet, lowWaterMark), nil
}

// Ready implements the health.Checker interface the facilitator's /healthz
// and /settle readiness gate (T7) use.
func (r *ReadinessChecker) Ready(ctx context.Context) error {
	return CheckReady(ctx, r.eth, r.gasWallet, r.lowWaterMark)
}
