package httpapi

import (
	"encoding/hex"
	"errors"
	"math/big"
	"strings"

	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	"golang.org/x/crypto/sha3"

	"github.com/zerkerlabs/farcaster/x402types"
)

// EIP-712 domain for native USDC on Base (spec 0005 Decision 3). The
// verifyingContract is the same token advertised as the x402 `asset`
// (x402USDCBaseAsset), so a signature valid over this domain is — by
// construction — an authorization to transfer USDC on Base. That is how the
// asset is bound at verification time without any on-chain read (Decisions 2, 9).
//
// name/version match Circle's FiatToken deployment (USDC uses domain version
// "2" for EIP-3009 / transferWithAuthorization); chainId is Base mainnet.
// v1 is gate-only, so if any of these ever mismatched the live contract the
// effect is over-rejection (fail-closed), never over-acceptance — validate
// against a real signature vector before settlement/collection is added.
const (
	usdcBaseDomainName    = "USD Coin"
	usdcBaseDomainVersion = "2"
	baseChainID           = 8453
)

// EIP-712 type strings whose keccak256 are the type hashes.
var (
	eip712DomainType     = []byte("EIP712Domain(string name,string version,uint256 chainId,address verifyingContract)")
	transferWithAuthType = []byte("TransferWithAuthorization(address from,address to,uint256 value,uint256 validAfter,uint256 validBefore,bytes32 nonce)")
)

var (
	errSigFormat   = errors.New("x402: malformed signature")
	errSigRecover  = errors.New("x402: signature recovery failed")
	errSigMismatch = errors.New("x402: signer does not match authorization.from")
	errAuthzField  = errors.New("x402: malformed authorization field")
)

// keccak256 returns the Keccak-256 hash (the Ethereum variant, not NIST
// SHA3-256) of the concatenation of parts.
func keccak256(parts ...[]byte) []byte {
	h := sha3.NewLegacyKeccak256()
	for _, p := range parts {
		h.Write(p)
	}
	return h.Sum(nil)
}

// decodeHexFixed decodes a 0x-prefixed hex string that must decode to exactly n
// bytes (e.g. 20 for an address, 32 for a nonce, 65 for a signature).
func decodeHexFixed(s string, n int) ([]byte, error) {
	b, err := hex.DecodeString(strings.TrimPrefix(s, "0x"))
	if err != nil || len(b) != n {
		return nil, errAuthzField
	}
	return b, nil
}

// addressWord parses a 20-byte hex address and left-pads it into a 32-byte
// EIP-712 word (the address occupies the low 20 bytes).
func addressWord(addr string) ([]byte, error) {
	b, err := decodeHexFixed(addr, 20)
	if err != nil {
		return nil, err
	}
	w := make([]byte, 32)
	copy(w[12:], b)
	return w, nil
}

// uintWord parses a base-10 uint256 string into a 32-byte big-endian word. A
// negative value or one wider than 256 bits is rejected.
func uintWord(dec string) ([]byte, error) {
	n, ok := new(big.Int).SetString(dec, 10)
	if !ok || n.Sign() < 0 || n.BitLen() > 256 {
		return nil, errAuthzField
	}
	w := make([]byte, 32)
	n.FillBytes(w)
	return w, nil
}

// bytes32Word parses a 0x-prefixed 32-byte hex value (the EIP-3009 nonce).
func bytes32Word(s string) ([]byte, error) {
	return decodeHexFixed(s, 32)
}

// eip3009Digest computes the EIP-712 signing digest for the EIP-3009
// transferWithAuthorization described by authz, over the USDC-on-Base domain:
//
//	keccak256(0x19 0x01 || domainSeparator || hashStruct(message))
func eip3009Digest(authz x402types.Authorization) ([]byte, error) {
	contract, err := addressWord(x402USDCBaseAsset)
	if err != nil {
		return nil, err
	}
	domainSeparator := keccak256(
		keccak256(eip712DomainType),
		keccak256([]byte(usdcBaseDomainName)),
		keccak256([]byte(usdcBaseDomainVersion)),
		uint256Word(baseChainID),
		contract,
	)

	from, err := addressWord(authz.From)
	if err != nil {
		return nil, err
	}
	to, err := addressWord(authz.To)
	if err != nil {
		return nil, err
	}
	value, err := uintWord(authz.Value)
	if err != nil {
		return nil, err
	}
	validAfter, err := uintWord(authz.ValidAfter)
	if err != nil {
		return nil, err
	}
	validBefore, err := uintWord(authz.ValidBefore)
	if err != nil {
		return nil, err
	}
	nonce, err := bytes32Word(authz.Nonce)
	if err != nil {
		return nil, err
	}
	structHash := keccak256(
		keccak256(transferWithAuthType),
		from, to, value, validAfter, validBefore, nonce,
	)

	return keccak256([]byte{0x19, 0x01}, domainSeparator, structHash), nil
}

// uint256Word encodes a small non-negative int into a 32-byte big-endian word
// (used for the constant chainId).
func uint256Word(v int64) []byte {
	w := make([]byte, 32)
	big.NewInt(v).FillBytes(w)
	return w
}

// recoverEthAddress recovers the Ethereum address that produced an EIP-712
// signature over digest. sig is the 65-byte Ethereum signature r(32)||s(32)||v(1)
// where v is 27/28 (or 0/1). The address is the low 20 bytes of the keccak256 of
// the uncompressed public key (sans the 0x04 prefix).
func recoverEthAddress(digest, sig []byte) (string, error) {
	if len(sig) != 65 {
		return "", errSigFormat
	}
	v := sig[64]
	var recID byte
	switch v {
	case 27, 28:
		recID = v - 27
	case 0, 1:
		recID = v
	default:
		return "", errSigFormat
	}
	// decred's RecoverCompact wants a Bitcoin-style compact signature with the
	// recovery code in the FIRST byte (27 + recID for an uncompressed key),
	// followed by R and S — the inverse byte order from Ethereum's r||s||v.
	compact := make([]byte, 65)
	compact[0] = 27 + recID
	copy(compact[1:33], sig[0:32])
	copy(compact[33:65], sig[32:64])

	pub, _, err := ecdsa.RecoverCompact(compact, digest)
	if err != nil {
		return "", errSigRecover
	}
	// SerializeUncompressed returns 0x04 || X(32) || Y(32); the Ethereum address
	// is keccak256(X||Y)[12:].
	addr := keccak256(pub.SerializeUncompressed()[1:])[12:]
	return "0x" + hex.EncodeToString(addr), nil
}

// baseUSDCVerifier is the production PaymentVerifier: it verifies the caller's
// EIP-712 signature over the EIP-3009 transferWithAuthorization for USDC on Base
// (spec 0005 Decision 3). It performs no on-chain read and holds no key — it
// only recovers the off-chain signer and confirms it is the authorization's
// `from`. Solana (Decision 3 fast-follow) is a future sibling behind the same
// PaymentVerifier interface.
type baseUSDCVerifier struct{}

func (baseUSDCVerifier) VerifySignature(pmt *x402types.Payment, _ x402types.PaymentRequirements) error {
	digest, err := eip3009Digest(pmt.Payload.Authorization)
	if err != nil {
		return err
	}
	sig, err := decodeHexFixed(pmt.Payload.Signature, 65)
	if err != nil {
		return errSigFormat
	}
	signer, err := recoverEthAddress(digest, sig)
	if err != nil {
		return err
	}
	// The token holder (`from`) is who signs an EIP-3009 authorization.
	if !strings.EqualFold(signer, pmt.Payload.Authorization.From) {
		return errSigMismatch
	}
	return nil
}
