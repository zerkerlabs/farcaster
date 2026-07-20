// Package chain is the facilitator's on-chain money path (spec 0007 T3): it
// builds, signs, and broadcasts the EIP-3009 transferWithAuthorization that
// moves a payer's USDC to the operator's payTo, and waits for the transfer to
// land in a block before returning.
//
// Two things are deliberately separated here. Signing goes through the
// [Signer] interface (Decision 2): the managed deployment injects an AWS-KMS
// signer whose secp256k1 key never leaves the HSM (see [KMSSigner] and the
// awskms adapter), while a sovereign self-hoster supplies a [LocalSigner]
// backed by a local-encrypted keystore — the same binary, a swappable backend.
// The RPC is reached through the narrow [EthClient] interface so the chain
// logic is unit-testable against a mocked node with no live network.
//
// This module is where go-ethereum, the RPC client, and the KMS SDK live;
// they are quarantined here and must never appear in gateway/go.mod (ADR-0010).
package chain

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/accounts/keystore"
	"github.com/ethereum/go-ethereum/common"
	ethcrypto "github.com/ethereum/go-ethereum/crypto"
)

// Signer produces an Ethereum secp256k1 signature over a 32-byte hash. It is
// the single custody boundary for the facilitator's gas key (spec 0007
// Decision 2): callers can learn the gas-wallet [Signer.Address] and obtain
// signatures, but never the private key itself — in the managed deployment
// the key lives in AWS KMS and is never in process memory, never logged, and
// never returned.
type Signer interface {
	// Address is the Ethereum address of the gas wallet this Signer controls
	// (derived from its public key). Transactions are submitted from it and
	// it pays gas; it holds no key over anyone's funds.
	Address() common.Address

	// SignHash signs a 32-byte hash and returns a 65-byte Ethereum signature
	// in r(32) || s(32) || v(1) order, where v is the recovery id in {0, 1}
	// and s is canonical (low-S, EIP-2). This is the exact shape
	// [github.com/ethereum/go-ethereum/core/types.Transaction.WithSignature]
	// expects for a typed (EIP-1559) transaction. It errors rather than
	// return a partial or malformed signature.
	SignHash(ctx context.Context, hash []byte) ([]byte, error)
}

// ErrSignHashLength is returned by a Signer when asked to sign something other
// than a 32-byte hash — signers operate on the pre-hashed digest only.
var ErrSignHashLength = errors.New("chain: hash to sign must be 32 bytes")

// LocalSigner is the self-hostable [Signer]: a secp256k1 key held in process,
// loaded from a local-encrypted Web3 keystore (see [NewLocalSignerFromKeystore])
// so a sovereign operator can run the same facilitator binary without AWS KMS.
// It is the escape hatch off the managed KMS path, not the managed default —
// the managed deployment uses [KMSSigner].
type LocalSigner struct {
	priv *ecdsa.PrivateKey
	addr common.Address
}

// NewLocalSigner builds a LocalSigner from an already-decrypted secp256k1
// private key. Prefer [NewLocalSignerFromKeystore] in production so the key
// only ever exists on disk encrypted; this constructor exists for callers
// that source the key another way (and for tests).
func NewLocalSigner(priv *ecdsa.PrivateKey) *LocalSigner {
	return &LocalSigner{
		priv: priv,
		addr: ethcrypto.PubkeyToAddress(priv.PublicKey),
	}
}

// NewLocalSignerFromKeystore decrypts a Web3 Secret Storage keystore JSON (the
// standard local-encrypted key format, as produced by geth/`clef`) with the
// given passphrase and returns a LocalSigner for it. This is the sovereign
// operator's on-ramp: their gas key lives on disk encrypted and is decrypted
// once at startup. The decrypted key stays inside the returned signer.
func NewLocalSignerFromKeystore(keyJSON []byte, passphrase string) (*LocalSigner, error) {
	key, err := keystore.DecryptKey(keyJSON, passphrase)
	if err != nil {
		// Do not wrap: keystore errors can be sensitive (they distinguish a
		// bad passphrase from a malformed file); a coarse error is enough.
		return nil, errors.New("chain: decrypt keystore failed")
	}
	return NewLocalSigner(key.PrivateKey), nil
}

// Address returns the gas-wallet address derived from the key.
func (s *LocalSigner) Address() common.Address { return s.addr }

// SignHash signs the 32-byte hash with the in-process key. go-ethereum's
// crypto.Sign already returns r||s||v with v in {0,1} and canonical low-S, so
// the output satisfies the [Signer] contract directly.
func (s *LocalSigner) SignHash(_ context.Context, hash []byte) ([]byte, error) {
	if len(hash) != 32 {
		return nil, ErrSignHashLength
	}
	sig, err := ethcrypto.Sign(hash, s.priv)
	if err != nil {
		return nil, fmt.Errorf("chain: local sign: %w", err)
	}
	return sig, nil
}
