package policy_test

import (
	"context"
	"errors"
	"testing"

	"github.com/zerkerlabs/farcaster/gateway/internal/policy"
)

const (
	tenantA = "tenant-alpha"
	tenantB = "tenant-beta"
)

func i64(n int64) *int64 { return &n }

func TestMemoryStore_GetAbsentReturnsNotFound(t *testing.T) {
	t.Parallel()

	s := policy.NewMemoryStore()
	if _, err := s.Get(context.Background(), tenantA); !errors.Is(err, policy.ErrNotFound) {
		t.Errorf("Get() error = %v, want ErrNotFound", err)
	}
}

func TestMemoryStore_PutCreatesOnFirstCall(t *testing.T) {
	t.Parallel()

	s := policy.NewMemoryStore()
	fields := policy.PutFields{
		Default: policy.ActionAllow,
		OnError: policy.ActionDeny,
		Rules: []policy.Rule{
			{Action: policy.ActionDeny, Match: policy.Match{Tools: []string{"delete_*"}}},
			{Action: policy.ActionWarn, Match: policy.Match{MaxBodyBytes: i64(32768)}},
		},
	}

	p, err := s.Put(context.Background(), tenantA, fields)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if p.TenantID != tenantA {
		t.Errorf("TenantID = %q, want %q", p.TenantID, tenantA)
	}
	if p.Version != 1 {
		t.Errorf("Version = %d, want 1", p.Version)
	}
	if p.Default != policy.ActionAllow || p.OnError != policy.ActionDeny {
		t.Errorf("Default/OnError = %q/%q", p.Default, p.OnError)
	}
	if len(p.Rules) != 2 {
		t.Fatalf("len(Rules) = %d, want 2", len(p.Rules))
	}
	if p.CreatedAt.IsZero() || p.UpdatedAt.IsZero() {
		t.Error("CreatedAt/UpdatedAt not set")
	}

	got, err := s.Get(context.Background(), tenantA)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Version != p.Version || len(got.Rules) != len(p.Rules) {
		t.Errorf("Get() = %+v, want match with created %+v", got, p)
	}
}

func TestMemoryStore_PutReplacesAndIncrementsVersion(t *testing.T) {
	t.Parallel()

	s := policy.NewMemoryStore()
	if _, err := s.Put(context.Background(), tenantA, policy.PutFields{
		Default: policy.ActionAllow,
		OnError: policy.ActionDeny,
		Rules: []policy.Rule{
			{Action: policy.ActionDeny, Match: policy.Match{Tools: []string{"delete_*"}}},
		},
	}); err != nil {
		t.Fatalf("Put (create): %v", err)
	}

	updated, err := s.Put(context.Background(), tenantA, policy.PutFields{
		Default: policy.ActionDeny,
		OnError: policy.ActionDeny,
		Rules:   nil,
	})
	if err != nil {
		t.Fatalf("Put (replace): %v", err)
	}
	if updated.Version != 2 {
		t.Errorf("Version = %d, want 2", updated.Version)
	}
	if updated.Default != policy.ActionDeny {
		t.Errorf("Default = %q, want %q", updated.Default, policy.ActionDeny)
	}
	if len(updated.Rules) != 0 {
		t.Errorf("Rules = %+v, want empty (PUT is a wholesale replace)", updated.Rules)
	}
}

func TestMemoryStore_PutDoesNotAliasCallerSlice(t *testing.T) {
	t.Parallel()

	s := policy.NewMemoryStore()
	rules := []policy.Rule{{Action: policy.ActionAllow, Match: policy.Match{Agents: []string{"agt_1"}}}}
	if _, err := s.Put(context.Background(), tenantA, policy.PutFields{
		Default: policy.ActionAllow,
		OnError: policy.ActionDeny,
		Rules:   rules,
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	rules[0].Action = policy.ActionDeny

	got, err := s.Get(context.Background(), tenantA)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Rules[0].Action != policy.ActionAllow {
		t.Errorf("Rules[0].Action = %q, want %q (mutating caller slice leaked into store)", got.Rules[0].Action, policy.ActionAllow)
	}
}

func TestMemoryStore_TenantIsolation(t *testing.T) {
	t.Parallel()

	s := policy.NewMemoryStore()
	if _, err := s.Put(context.Background(), tenantA, policy.PutFields{
		Default: policy.ActionAllow,
		OnError: policy.ActionDeny,
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	if _, err := s.Get(context.Background(), tenantB); !errors.Is(err, policy.ErrNotFound) {
		t.Errorf("Get(tenantB) error = %v, want ErrNotFound", err)
	}
}
