package chain

import (
	"context"
	"errors"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func TestCheckReady_RPCUnreachable(t *testing.T) {
	eth := newMockEth()
	eth.headErr = errors.New("dial tcp: connection refused")

	err := CheckReady(context.Background(), eth, common.Address{}, big.NewInt(0))
	if !errors.Is(err, ErrRPCUnreachable) {
		t.Fatalf("err = %v, want ErrRPCUnreachable", err)
	}
}

func TestCheckReady_BalanceFetchFails(t *testing.T) {
	eth := newMockEth()
	eth.balanceErr = errors.New("rpc: balance read failed")

	err := CheckReady(context.Background(), eth, common.Address{}, big.NewInt(0))
	if !errors.Is(err, ErrRPCUnreachable) {
		t.Fatalf("err = %v, want ErrRPCUnreachable", err)
	}
}

func TestCheckReady_GasBalanceLow(t *testing.T) {
	eth := newMockEth()
	eth.balance = big.NewInt(1)

	err := CheckReady(context.Background(), eth, common.Address{}, big.NewInt(2))
	if !errors.Is(err, ErrGasBalanceLow) {
		t.Fatalf("err = %v, want ErrGasBalanceLow", err)
	}
}

func TestCheckReady_Ready(t *testing.T) {
	eth := newMockEth()
	eth.balance = big.NewInt(10)

	if err := CheckReady(context.Background(), eth, common.Address{}, big.NewInt(2)); err != nil {
		t.Fatalf("CheckReady() = %v, want nil", err)
	}
}

func TestCheckReady_BalanceEqualsLowWaterMark(t *testing.T) {
	eth := newMockEth()
	eth.balance = big.NewInt(2)

	if err := CheckReady(context.Background(), eth, common.Address{}, big.NewInt(2)); err != nil {
		t.Fatalf("CheckReady() = %v, want nil (balance == low-water mark is ready)", err)
	}
}

func TestReadinessChecker_Ready(t *testing.T) {
	eth := newMockEth()
	eth.balance = big.NewInt(10)

	rc := NewReadinessChecker(eth, common.Address{}, big.NewInt(2))
	if err := rc.Ready(context.Background()); err != nil {
		t.Fatalf("Ready() = %v, want nil", err)
	}
}

func TestReadinessChecker_NotReady(t *testing.T) {
	eth := newMockEth()
	eth.balance = big.NewInt(1)

	rc := NewReadinessChecker(eth, common.Address{}, big.NewInt(2))
	if err := rc.Ready(context.Background()); !errors.Is(err, ErrGasBalanceLow) {
		t.Fatalf("Ready() = %v, want ErrGasBalanceLow", err)
	}
}
