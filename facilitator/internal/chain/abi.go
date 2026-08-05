package chain

import (
	"encoding/hex"
	"errors"
	"math/big"
	"strings"

	ethcrypto "github.com/ethereum/go-ethereum/crypto"

	"github.com/zerkerlabs/gateway/x402types"
)

// Malformed-input rejections from calldata encoding. By the time /settle calls
// Submit the authorization has already passed re-verification (T2), but the
// chain client parses defensively and never broadcasts a transaction built
// from a field it could not encode.
var (
	ErrMalformedAuthorization = errors.New("chain: malformed authorization field")
	ErrMalformedSignature     = errors.New("chain: malformed payment signature")
)

// transferWithAuthorizationSelector is the 4-byte function selector for the
// EIP-3009 (v, r, s) form the spec pins:
//
//	transferWithAuthorization(address,address,uint256,uint256,uint256,bytes32,uint8,bytes32,bytes32)
//
// Computed once at init from the canonical signature so it cannot drift from a
// hand-copied hex constant.
var transferWithAuthorizationSelector = ethcrypto.Keccak256(
	[]byte("transferWithAuthorization(address,address,uint256,uint256,uint256,bytes32,uint8,bytes32,bytes32)"),
)[:4]

// encodeTransferWithAuthorization builds the calldata for
// transferWithAuthorization from the payer-signed authorization and the
// payer's 65-byte signature. Every argument is a static 32-byte ABI word, so
// the calldata is the selector followed by nine words in declaration order:
// from, to, value, validAfter, validBefore, nonce, v, r, s.
//
// The (v, r, s) passed on-chain are the *payer's* signature over the EIP-3009
// authorization (recovered and checked in T2) — not the gas wallet's tx
// signature. USDC's on-chain ecrecover expects v in {27, 28}, so a signature
// carrying the {0, 1} recovery id is normalized up.
func encodeTransferWithAuthorization(authz x402types.Authorization, signature string) ([]byte, error) {
	from, err := addressWord(authz.From)
	if err != nil {
		return nil, err
	}
	to, err := addressWord(authz.To)
	if err != nil {
		return nil, err
	}
	value, err := uint256Word(authz.Value)
	if err != nil {
		return nil, err
	}
	validAfter, err := uint256Word(authz.ValidAfter)
	if err != nil {
		return nil, err
	}
	validBefore, err := uint256Word(authz.ValidBefore)
	if err != nil {
		return nil, err
	}
	nonce, err := bytes32Word(authz.Nonce)
	if err != nil {
		return nil, err
	}
	v, r, s, err := splitSignature(signature)
	if err != nil {
		return nil, err
	}

	calldata := make([]byte, 0, 4+9*32)
	calldata = append(calldata, transferWithAuthorizationSelector...)
	calldata = append(calldata, from...)
	calldata = append(calldata, to...)
	calldata = append(calldata, value...)
	calldata = append(calldata, validAfter...)
	calldata = append(calldata, validBefore...)
	calldata = append(calldata, nonce...)
	calldata = append(calldata, v...)
	calldata = append(calldata, r...)
	calldata = append(calldata, s...)
	return calldata, nil
}

// decodeHexFixed decodes a 0x-prefixed hex string that must be exactly n bytes.
func decodeHexFixed(s string, n int) ([]byte, error) {
	b, err := hex.DecodeString(strings.TrimPrefix(s, "0x"))
	if err != nil || len(b) != n {
		return nil, ErrMalformedAuthorization
	}
	return b, nil
}

// addressWord parses a 20-byte hex address into a left-padded 32-byte word.
func addressWord(addr string) ([]byte, error) {
	b, err := decodeHexFixed(addr, 20)
	if err != nil {
		return nil, err
	}
	w := make([]byte, 32)
	copy(w[12:], b)
	return w, nil
}

// uint256Word parses a base-10 uint256 string into a 32-byte big-endian word,
// rejecting negatives and values wider than 256 bits.
func uint256Word(dec string) ([]byte, error) {
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

// splitSignature decomposes the payer's 65-byte r||s||v signature into the
// three ABI words the contract call takes: v (a uint8 in the low byte of a
// word, normalized to 27/28) and the 32-byte r and s. A signature that is not
// exactly 65 bytes or carries an out-of-range v is rejected.
func splitSignature(signature string) (v, r, s []byte, err error) {
	raw, err := decodeHexFixed(signature, 65)
	if err != nil {
		return nil, nil, nil, ErrMalformedSignature
	}
	vByte := raw[64]
	switch vByte {
	case 0, 1:
		vByte += 27
	case 27, 28:
		// already the on-chain convention
	default:
		return nil, nil, nil, ErrMalformedSignature
	}
	vWord := make([]byte, 32)
	vWord[31] = vByte
	return vWord, raw[0:32], raw[32:64], nil
}
