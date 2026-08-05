package chain

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"time"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/zerkerlabs/gateway/x402types"
)

// Submission outcomes the /settle orchestration (T5) maps onto the wire. These
// are *outcome* / *transient* signals, distinct from a malformed-input error:
// they mean the transfer could not be landed, not that the caller sent garbage.
var (
	// ErrInclusionTimeout means the transaction was broadcast but did not
	// appear in a block before the inclusion deadline. It is transient — the
	// transaction may still land — so the caller reports a transient failure
	// and may retry (idempotency dedupe, T4, keeps a retry from double-paying).
	ErrInclusionTimeout = errors.New("chain: transaction not included before timeout")

	// ErrReverted means the transaction was included but reverted on-chain
	// (e.g. the payer's balance was insufficient at submit time, or the
	// authorization nonce was already consumed). It is a collection outcome,
	// not a protocol error.
	ErrReverted = errors.New("chain: transaction reverted on-chain")

	// ErrGasEstimation means the node rejected the transaction at estimate
	// time — almost always because it would revert. Treating it as an outcome
	// failure avoids broadcasting (and paying gas for) a doomed transaction.
	ErrGasEstimation = errors.New("chain: gas estimation failed (transfer would revert)")

	// ErrNoBaseFee means the chain head carried no EIP-1559 base fee. The
	// facilitator only settles on 1559-enabled networks (Base), so this is a
	// misconfiguration rather than an expected outcome.
	ErrNoBaseFee = errors.New("chain: chain head has no EIP-1559 base fee")
)

// EthClient is the narrow slice of an Ethereum JSON-RPC node the chain client
// uses. *ethclient.Client satisfies it; tests supply a mock so the submit path
// runs with no live network (spec 0007 T3 scope: unit tests with mocked RPC).
type EthClient interface {
	PendingNonceAt(ctx context.Context, account common.Address) (uint64, error)
	SuggestGasTipCap(ctx context.Context) (*big.Int, error)
	HeaderByNumber(ctx context.Context, number *big.Int) (*types.Header, error)
	EstimateGas(ctx context.Context, call ethereum.CallMsg) (uint64, error)
	SendTransaction(ctx context.Context, tx *types.Transaction) error
	TransactionReceipt(ctx context.Context, txHash common.Hash) (*types.Receipt, error)

	// BalanceAt returns account's balance (wei) at the given block, or the
	// latest block when number is nil. Used by [CheckReady] for the
	// /healthz gas-wallet low-water-mark check (spec 0007 T7).
	BalanceAt(ctx context.Context, account common.Address, number *big.Int) (*big.Int, error)

	// CallContract runs a read-only contract call. Used by [Client.Reconcile]
	// (spec 0007 follow-up #179) to read the USDC contract's
	// authorizationState(address,bytes32) for a stuck in_flight row's
	// authorization.
	CallContract(ctx context.Context, call ethereum.CallMsg, blockNumber *big.Int) ([]byte, error)

	// FilterLogs returns event logs matching query. Used by [Client.Reconcile]
	// to locate the AuthorizationUsed log (and its transaction hash) for an
	// authorization whose nonce is already consumed on-chain.
	FilterLogs(ctx context.Context, query ethereum.FilterQuery) ([]types.Log, error)
}

// Config tunes a Client. USDC is required (the asset contract this client
// submits transfers to, per-network); the timing fields default if left zero.
type Config struct {
	// USDC is the USDC contract address on this client's network — the `to`
	// of the on-chain transaction (the token move happens inside it).
	USDC common.Address

	// InclusionTimeout bounds the wait for first-block inclusion (Decision 6)
	// before the submission is reported transient. Defaults to 60s.
	InclusionTimeout time.Duration

	// PollInterval is how often the receipt is polled while awaiting
	// inclusion. Defaults to 2s (≈ Base block time).
	PollInterval time.Duration

	// ReconcileLookbackBlocks bounds how far back [Client.Reconcile] searches
	// chain logs for the AuthorizationUsed event once it finds a nonce
	// consumed: the settling transaction, if any, broadcast at most a few
	// inclusion timeouts before a row went stale, never years ago, so an
	// unbounded eth_getLogs scan back to genesis would be wasted RPC load.
	// Defaults to 200_000 blocks (~4.6 days at Base's ~2s block time).
	ReconcileLookbackBlocks uint64
}

const (
	defaultInclusionTimeout        = 60 * time.Second
	defaultPollInterval            = 2 * time.Second
	defaultReconcileLookbackBlocks = 200_000
)

// Client submits EIP-3009 transferWithAuthorization transactions from a single
// gas wallet on a single network. It is the facilitator's on-chain writer:
// one Client per network, bound to that network's chain id, USDC contract, and
// gas-key [Signer]. It is safe for concurrent use — gas-wallet nonces are
// allocated sequentially under a lock so parallel settles never collide or
// gap the nonce sequence.
type Client struct {
	eth      EthClient
	signer   Signer
	chainID  *big.Int
	txSigner types.Signer
	usdc     common.Address

	inclusionTimeout  time.Duration
	pollInterval      time.Duration
	reconcileLookback uint64

	// mu guards the gas-wallet nonce counter and serializes broadcasts so a
	// failed send can roll the counter back without racing a later send.
	mu        sync.Mutex
	nextNonce uint64
	nonceInit bool
}

// NewClient builds a Client over the given node, signer, and chain id.
func NewClient(eth EthClient, signer Signer, chainID *big.Int, cfg Config) *Client {
	if cfg.InclusionTimeout <= 0 {
		cfg.InclusionTimeout = defaultInclusionTimeout
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = defaultPollInterval
	}
	if cfg.ReconcileLookbackBlocks == 0 {
		cfg.ReconcileLookbackBlocks = defaultReconcileLookbackBlocks
	}
	return &Client{
		eth:               eth,
		signer:            signer,
		chainID:           chainID,
		txSigner:          types.LatestSignerForChainID(chainID),
		usdc:              cfg.USDC,
		inclusionTimeout:  cfg.InclusionTimeout,
		pollInterval:      cfg.PollInterval,
		reconcileLookback: cfg.ReconcileLookbackBlocks,
	}
}

// Dial connects to an Ethereum JSON-RPC endpoint and returns a Client over it.
// It is the production constructor (the RPC config entry point); tests use
// [NewClient] with a mock EthClient instead.
func Dial(ctx context.Context, rpcURL string, signer Signer, chainID *big.Int, cfg Config) (*Client, error) {
	ec, err := ethclient.DialContext(ctx, rpcURL)
	if err != nil {
		return nil, fmt.Errorf("chain: dial rpc: %w", err)
	}
	return NewClient(ec, signer, chainID, cfg), nil
}

// GasWallet returns the address transactions are submitted from — useful for
// the /healthz gas-balance check (T7) and for logging.
func (c *Client) GasWallet() common.Address { return c.signer.Address() }

// Submit builds, signs, and broadcasts transferWithAuthorization for the given
// payer authorization and payer signature, then waits for first-block
// inclusion (Decision 6). On success it returns the transaction hash and a nil
// error; the returned hash is also set on the outcome errors ([ErrReverted],
// [ErrInclusionTimeout]) so the caller can still record it. A malformed
// authorization/signature, or an RPC/protocol failure, returns a zero hash.
func (c *Client) Submit(ctx context.Context, authz x402types.Authorization, signature string) (common.Hash, error) {
	calldata, err := encodeTransferWithAuthorization(authz, signature)
	if err != nil {
		return common.Hash{}, err
	}

	tipCap, feeCap, err := c.suggestFees(ctx)
	if err != nil {
		return common.Hash{}, err
	}

	gasLimit, err := c.estimateGas(ctx, calldata)
	if err != nil {
		return common.Hash{}, err
	}

	txHash, err := c.broadcast(ctx, calldata, tipCap, feeCap, gasLimit)
	if err != nil {
		return common.Hash{}, err
	}

	receipt, err := c.awaitInclusion(ctx, txHash)
	if err != nil {
		return txHash, err
	}
	if receipt.Status != types.ReceiptStatusSuccessful {
		return txHash, ErrReverted
	}
	return txHash, nil
}

// suggestFees builds EIP-1559 fee caps: the tip from the node's suggestion, and
// a max fee of 2×baseFee + tip so the transaction stays includable across a
// couple of base-fee increases.
func (c *Client) suggestFees(ctx context.Context) (tipCap, feeCap *big.Int, err error) {
	tipCap, err = c.eth.SuggestGasTipCap(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("chain: suggest gas tip: %w", err)
	}
	head, err := c.eth.HeaderByNumber(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("chain: fetch head: %w", err)
	}
	if head.BaseFee == nil {
		return nil, nil, ErrNoBaseFee
	}
	feeCap = new(big.Int).Add(new(big.Int).Mul(head.BaseFee, big.NewInt(2)), tipCap)
	return tipCap, feeCap, nil
}

// estimateGas estimates the transfer's gas and pads it by 25% for the headroom
// between estimate and inclusion. A node rejection here means the transaction
// would revert, so it maps to the outcome error [ErrGasEstimation].
func (c *Client) estimateGas(ctx context.Context, calldata []byte) (uint64, error) {
	from := c.signer.Address()
	gas, err := c.eth.EstimateGas(ctx, ethereum.CallMsg{
		From: from,
		To:   &c.usdc,
		Data: calldata,
	})
	if err != nil {
		return 0, fmt.Errorf("%w: %w", ErrGasEstimation, err)
	}
	return gas + gas/4, nil
}

// broadcast reserves the next gas-wallet nonce, signs, and sends — all under
// the lock so that a failed send rolls the nonce counter back cleanly without
// racing a concurrent send. The inclusion wait happens outside the lock.
func (c *Client) broadcast(ctx context.Context, calldata []byte, tipCap, feeCap *big.Int, gasLimit uint64) (common.Hash, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	nonce, err := c.reserveNonceLocked(ctx)
	if err != nil {
		return common.Hash{}, err
	}

	tx := types.NewTx(&types.DynamicFeeTx{
		ChainID:   c.chainID,
		Nonce:     nonce,
		GasTipCap: tipCap,
		GasFeeCap: feeCap,
		Gas:       gasLimit,
		To:        &c.usdc,
		Value:     new(big.Int),
		Data:      calldata,
	})

	signed, err := c.signTx(ctx, tx)
	if err != nil {
		c.nonceInit = false // resync on next attempt
		return common.Hash{}, err
	}
	if err := c.eth.SendTransaction(ctx, signed); err != nil {
		c.nonceInit = false // this nonce was not consumed on-chain
		return common.Hash{}, fmt.Errorf("chain: broadcast: %w", err)
	}
	c.nextNonce = nonce + 1
	return signed.Hash(), nil
}

// reserveNonceLocked returns the gas wallet's next nonce, seeding the counter
// from the node (pending pool included) on first use. Must hold c.mu.
func (c *Client) reserveNonceLocked(ctx context.Context) (uint64, error) {
	if !c.nonceInit {
		n, err := c.eth.PendingNonceAt(ctx, c.signer.Address())
		if err != nil {
			return 0, fmt.Errorf("chain: fetch nonce: %w", err)
		}
		c.nextNonce = n
		c.nonceInit = true
	}
	return c.nextNonce, nil
}

// signTx computes the typed-transaction signing hash, signs it through the
// custody [Signer] (KMS/local), and attaches the signature.
func (c *Client) signTx(ctx context.Context, tx *types.Transaction) (*types.Transaction, error) {
	h := c.txSigner.Hash(tx)
	sig, err := c.signer.SignHash(ctx, h.Bytes())
	if err != nil {
		return nil, fmt.Errorf("chain: sign tx: %w", err)
	}
	signed, err := tx.WithSignature(c.txSigner, sig)
	if err != nil {
		return nil, fmt.Errorf("chain: attach signature: %w", err)
	}
	return signed, nil
}

// awaitInclusion polls for the transaction receipt until the transaction is
// mined or the inclusion timeout elapses (Decision 6). A missing receipt is
// "still pending"; a genuine RPC error is returned as-is; the deadline maps to
// [ErrInclusionTimeout].
func (c *Client) awaitInclusion(ctx context.Context, txHash common.Hash) (*types.Receipt, error) {
	ctx, cancel := context.WithTimeout(ctx, c.inclusionTimeout)
	defer cancel()

	ticker := time.NewTicker(c.pollInterval)
	defer ticker.Stop()

	for {
		receipt, err := c.eth.TransactionReceipt(ctx, txHash)
		switch {
		case err == nil:
			return receipt, nil
		case errors.Is(err, ethereum.NotFound):
			// not mined yet — keep polling
		case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
			return nil, ErrInclusionTimeout
		default:
			return nil, fmt.Errorf("chain: fetch receipt: %w", err)
		}

		select {
		case <-ctx.Done():
			return nil, ErrInclusionTimeout
		case <-ticker.C:
		}
	}
}
