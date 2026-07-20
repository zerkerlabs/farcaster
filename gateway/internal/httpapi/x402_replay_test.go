package httpapi

import (
	"testing"
	"time"
)

func TestNonceStore_Reserve(t *testing.T) {
	t.Parallel()

	t.Run("fresh pair is accepted then flagged as replay", func(t *testing.T) {
		t.Parallel()
		s := newNonceStore()
		now := time.Unix(1700000000, 0).UTC()
		if s.reserve("0xPayer", "0xNonce", now) {
			t.Fatal("first reserve reported a replay, want fresh")
		}
		if !s.reserve("0xPayer", "0xNonce", now) {
			t.Fatal("second reserve of the same pair did not report a replay")
		}
	})

	t.Run("distinct nonce or payer is independent", func(t *testing.T) {
		t.Parallel()
		s := newNonceStore()
		now := time.Unix(1700000000, 0).UTC()
		s.reserve("0xPayer", "0xNonce1", now)
		if s.reserve("0xPayer", "0xNonce2", now) {
			t.Error("different nonce, same payer reported as a replay")
		}
		if s.reserve("0xOtherPayer", "0xNonce1", now) {
			t.Error("different payer, same nonce reported as a replay")
		}
	})

	t.Run("comparison is case-insensitive on hex fields", func(t *testing.T) {
		t.Parallel()
		s := newNonceStore()
		now := time.Unix(1700000000, 0).UTC()
		s.reserve("0xABCDEF", "0x0011", now)
		if !s.reserve("0xabcdef", "0x0011", now) {
			t.Error("case-differing payer/nonce was not caught as a replay")
		}
	})

	t.Run("entry expires after the bounded TTL", func(t *testing.T) {
		t.Parallel()
		s := newNonceStore()
		start := time.Unix(1700000000, 0).UTC()
		s.reserve("0xPayer", "0xNonce", start)
		later := start.Add(nonceTTL + time.Second)
		if s.reserve("0xPayer", "0xNonce", later) {
			t.Error("entry past its TTL was still reported as a replay")
		}
	})
}
