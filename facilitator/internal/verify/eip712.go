package verify

import (
	"encoding/hex"
	"errors"
	"math/big"
	"strings"

	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	"golang.org/x/crypto/sha3"

	"github.com/zerkerlabs/gateway/x402types"
)

// Signature-recovery rejections. These are independent of the gateway's
// (gateway/internal/httpapi/x402_eip712.go) — same EIP-3009/EIP-712 math,
// deliberately re-implemented rather than imported (package doc).
var (
	ErrMalformedAuthorization = errors.New("facilitator: malformed authorization field")
	ErrSignatureFormat        = errors.New("facilitator: malformed signature")
	ErrSignatureRecovery      = errors.New("facilitator: signature recovery failed")
	ErrSignatureMismatch      = errors.New("facilitator: signer does not match authorization.from")
)

// EIP-712 type strings whose keccak256 are the type hashes.
var (
	eip712DomainType     = []byte("EIP712Domain(string name,string version,uint256 chainId,address verifyingContract)")
	transferWithAuthType = []byte("TransferWithAuthorization(address from,address to,uint256 value,uint256 validAfter,uint256 validBefore,bytes32 nonce)")
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

// decodeHexFixed decodes a 0x-prefixed hex string that must decode to exactly
// n bytes (e.g. 20 for an address, 32 for a nonce, 65 for a signature).
func decodeHexFixed(s string, n int) ([]byte, error) {
	b, err := hex.DecodeString(strings.TrimPrefix(s, "0x"))
	if err != nil || len(b) != n {
		return nil, ErrMalformedAuthorization
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
		return nil, ErrMalformedAuthorization
	}
	w := make([]byte, 32)
	n.FillBytes(w)
	return w, nil
}

// bytes32Word parses a 0x-prefixed 32-byte hex value (the EIP-3009 nonce).
func bytes32Word(s string) ([]byte, error) {
	return decodeHexFixed(s, 32)
}

// chainIDWord encodes a chain ID into a 32-byte big-endian word.
func chainIDWord(chainID int64) []byte {
	w := make([]byte, 32)
	big.NewInt(chainID).FillBytes(w)
	return w
}

// eip3009Digest computes the EIP-712 signing digest for the EIP-3009
// transferWithAuthorization described by authz, over kind's asset domain:
//
//	keccak256(0x19 0x01 || domainSeparator || hashStruct(message))
func eip3009Digest(authz x402types.Authorization, kind Kind) ([]byte, error) {
	contract, err := addressWord(kind.Asset)
	if err != nil {
		return nil, err
	}
	domainSeparator := keccak256(
		keccak256(eip712DomainType),
		keccak256([]byte(kind.AssetName)),
		keccak256([]byte(kind.AssetVersion)),
		chainIDWord(kind.ChainID),
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

// recoverEthAddress recovers the Ethereum address that produced an EIP-712
// signature over digest. sig is the 65-byte Ethereum signature
// r(32)||s(32)||v(1) where v is 27/28 (or 0/1). The address is the low 20
// bytes of the keccak256 of the uncompressed public key (sans the 0x04
// prefix).
func recoverEthAddress(digest, sig []byte) (string, error) {
	if len(sig) != 65 {
		return "", ErrSignatureFormat
	}
	v := sig[64]
	var recID byte
	switch v {
	case 27, 28:
		recID = v - 27
	case 0, 1:
		recID = v
	default:
		return "", ErrSignatureFormat
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
		return "", ErrSignatureRecovery
	}
	// SerializeUncompressed returns 0x04 || X(32) || Y(32); the Ethereum
	// address is keccak256(X||Y)[12:].
	addr := keccak256(pub.SerializeUncompressed()[1:])[12:]
	return "0x" + hex.EncodeToString(addr), nil
}

// verifySignature confirms the EIP-3009 signature recovers to authz.From,
// computing the digest over kind's asset domain.
func verifySignature(authz x402types.Authorization, signature string, kind Kind) error {
	digest, err := eip3009Digest(authz, kind)
	if err != nil {
		return err
	}
	sig, err := decodeHexFixed(signature, 65)
	if err != nil {
		return ErrSignatureFormat
	}
	signer, err := recoverEthAddress(digest, sig)
	if err != nil {
		return err
	}
	if !strings.EqualFold(signer, authz.From) {
		return ErrSignatureMismatch
	}
	return nil
}
