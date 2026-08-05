package policy_test

import (
	"testing"

	"github.com/zerkerlabs/gateway/gateway/internal/policy"
)

func strp(s string) *string { return &s }

func TestBuiltinEvaluator_NoPolicyAllows(t *testing.T) {
	t.Parallel()

	e := policy.NewBuiltinEvaluator()
	got := e.Evaluate(nil, policy.RequestContext{AgentID: "agt_1"})
	if got.Action != policy.ActionAllow {
		t.Errorf("Action = %q, want %q", got.Action, policy.ActionAllow)
	}
	if got.MatchedRule != "" {
		t.Errorf("MatchedRule = %q, want empty", got.MatchedRule)
	}
}

func TestBuiltinEvaluator_NoMatchAppliesDefault(t *testing.T) {
	t.Parallel()

	for _, def := range []policy.Action{policy.ActionAllow, policy.ActionWarn, policy.ActionDeny} {
		def := def
		t.Run(string(def), func(t *testing.T) {
			t.Parallel()

			p := &policy.Policy{
				Default: def,
				OnError: policy.ActionDeny,
				Rules: []policy.Rule{
					{Action: policy.ActionDeny, Match: policy.Match{Agents: []string{"agt_other"}}},
				},
			}
			e := policy.NewBuiltinEvaluator()
			got := e.Evaluate(p, policy.RequestContext{AgentID: "agt_mine"})
			if got.Action != def {
				t.Errorf("Action = %q, want %q", got.Action, def)
			}
			if got.MatchedRule != "" {
				t.Errorf("MatchedRule = %q, want empty (default, no rule matched)", got.MatchedRule)
			}
			if got.Reason == "" {
				t.Error("Reason is empty, want a non-empty explanation")
			}
		})
	}
}

func TestBuiltinEvaluator_EmptyRulesAppliesDefault(t *testing.T) {
	t.Parallel()

	p := &policy.Policy{Default: policy.ActionAllow, OnError: policy.ActionDeny}
	e := policy.NewBuiltinEvaluator()
	got := e.Evaluate(p, policy.RequestContext{AgentID: "agt_1"})
	if got.Action != policy.ActionAllow {
		t.Errorf("Action = %q, want %q", got.Action, policy.ActionAllow)
	}
}

func TestBuiltinEvaluator_RuleOrderingFirstMatchWins(t *testing.T) {
	t.Parallel()

	p := &policy.Policy{
		Default: policy.ActionAllow,
		OnError: policy.ActionDeny,
		Rules: []policy.Rule{
			{Action: policy.ActionWarn, Match: policy.Match{Agents: []string{"agt_1"}}},
			{Action: policy.ActionDeny, Match: policy.Match{Agents: []string{"agt_1"}}},
		},
	}
	e := policy.NewBuiltinEvaluator()
	got := e.Evaluate(p, policy.RequestContext{AgentID: "agt_1"})
	if got.Action != policy.ActionWarn {
		t.Errorf("Action = %q, want %q (first matching rule wins)", got.Action, policy.ActionWarn)
	}
	if got.MatchedRule != "1" {
		t.Errorf("MatchedRule = %q, want %q", got.MatchedRule, "1")
	}
}

func TestBuiltinEvaluator_AgentMatch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		agents  []string
		ctxAgt  string
		wantHit bool
	}{
		{"exact match", []string{"agt_1", "agt_2"}, "agt_1", true},
		{"no match", []string{"agt_1", "agt_2"}, "agt_3", false},
		{"empty agents matches any", nil, "agt_anything", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			p := &policy.Policy{
				Default: policy.ActionAllow,
				OnError: policy.ActionDeny,
				Rules: []policy.Rule{
					{Action: policy.ActionDeny, Match: policy.Match{Agents: tt.agents}},
				},
			}
			e := policy.NewBuiltinEvaluator()
			got := e.Evaluate(p, policy.RequestContext{AgentID: tt.ctxAgt})
			hit := got.Action == policy.ActionDeny
			if hit != tt.wantHit {
				t.Errorf("hit = %v, want %v (Action = %q)", hit, tt.wantHit, got.Action)
			}
		})
	}
}

func TestBuiltinEvaluator_ToolMatch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		tools    []string
		protocol string
		ctxTool  *string
		wantHit  bool
	}{
		{"exact tool match", []string{"delete_repo"}, policy.ProtocolMCP, strp("delete_repo"), true},
		{"exact tool no match", []string{"delete_repo"}, policy.ProtocolMCP, strp("read_repo"), false},
		{"wildcard match", []string{"delete_*"}, policy.ProtocolMCP, strp("delete_repo"), true},
		{"wildcard no match", []string{"delete_*"}, policy.ProtocolMCP, strp("read_repo"), false},
		{"nil tool never matches", []string{"delete_*"}, policy.ProtocolMCP, nil, false},
		{"http protocol never matches a tool condition", []string{"delete_*"}, policy.ProtocolHTTP, strp("delete_repo"), false},
		{"empty tools matches any", nil, policy.ProtocolMCP, strp("anything"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			p := &policy.Policy{
				Default: policy.ActionAllow,
				OnError: policy.ActionDeny,
				Rules: []policy.Rule{
					{Action: policy.ActionDeny, Match: policy.Match{Tools: tt.tools}},
				},
			}
			e := policy.NewBuiltinEvaluator()
			got := e.Evaluate(p, policy.RequestContext{Protocol: tt.protocol, MCPTool: tt.ctxTool})
			hit := got.Action == policy.ActionDeny
			if hit != tt.wantHit {
				t.Errorf("hit = %v, want %v (Action = %q)", hit, tt.wantHit, got.Action)
			}
		})
	}
}

func TestBuiltinEvaluator_HTTPAgentIsRouteLevelViaAgents(t *testing.T) {
	t.Parallel()

	// protocol=http has no tool concept (spec 0009 Decision 6): route-level
	// matching is expressed through Match.Agents (each proxy call is already
	// scoped to one agent ID), not Match.Tools.
	p := &policy.Policy{
		Default: policy.ActionAllow,
		OnError: policy.ActionDeny,
		Rules: []policy.Rule{
			{Action: policy.ActionDeny, Match: policy.Match{Agents: []string{"agt_http_1"}}},
		},
	}
	e := policy.NewBuiltinEvaluator()

	denied := e.Evaluate(p, policy.RequestContext{Protocol: policy.ProtocolHTTP, AgentID: "agt_http_1"})
	if denied.Action != policy.ActionDeny {
		t.Errorf("Action = %q, want %q", denied.Action, policy.ActionDeny)
	}

	allowed := e.Evaluate(p, policy.RequestContext{Protocol: policy.ProtocolHTTP, AgentID: "agt_http_2"})
	if allowed.Action != policy.ActionAllow {
		t.Errorf("Action = %q, want %q", allowed.Action, policy.ActionAllow)
	}
}

func TestBuiltinEvaluator_MaxBodyBytes(t *testing.T) {
	t.Parallel()

	limit := int64(32768)
	p := &policy.Policy{
		Default: policy.ActionAllow,
		OnError: policy.ActionDeny,
		Rules: []policy.Rule{
			{Action: policy.ActionWarn, Match: policy.Match{MaxBodyBytes: &limit}},
		},
	}
	e := policy.NewBuiltinEvaluator()

	tests := []struct {
		name      string
		bodyBytes int64
		wantHit   bool
	}{
		{"below limit", limit - 1, false},
		{"at limit (>=)", limit, true},
		{"above limit", limit + 1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := e.Evaluate(p, policy.RequestContext{BodyBytes: tt.bodyBytes})
			hit := got.Action == policy.ActionWarn
			if hit != tt.wantHit {
				t.Errorf("hit = %v, want %v (Action = %q)", hit, tt.wantHit, got.Action)
			}
		})
	}
}

func TestBuiltinEvaluator_RatePerMin(t *testing.T) {
	t.Parallel()

	limit := 60
	p := &policy.Policy{
		Default: policy.ActionAllow,
		OnError: policy.ActionDeny,
		Rules: []policy.Rule{
			{Action: policy.ActionDeny, Match: policy.Match{RatePerMin: &limit}},
		},
	}
	e := policy.NewBuiltinEvaluator()

	tests := []struct {
		name       string
		ratePerMin int
		wantHit    bool
	}{
		{"below limit", limit - 1, false},
		{"at limit (not exceeded)", limit, false},
		{"above limit (exceeds)", limit + 1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := e.Evaluate(p, policy.RequestContext{RatePerMin: tt.ratePerMin})
			hit := got.Action == policy.ActionDeny
			if hit != tt.wantHit {
				t.Errorf("hit = %v, want %v (Action = %q)", hit, tt.wantHit, got.Action)
			}
		})
	}
}

func TestBuiltinEvaluator_AllConditionsMustAllMatch(t *testing.T) {
	t.Parallel()

	limit := int64(1000)
	p := &policy.Policy{
		Default: policy.ActionAllow,
		OnError: policy.ActionDeny,
		Rules: []policy.Rule{
			{
				Action: policy.ActionDeny,
				Match: policy.Match{
					Agents:       []string{"agt_1"},
					Tools:        []string{"delete_*"},
					MaxBodyBytes: &limit,
				},
			},
		},
	}
	e := policy.NewBuiltinEvaluator()

	base := policy.RequestContext{
		Protocol:  policy.ProtocolMCP,
		AgentID:   "agt_1",
		MCPTool:   strp("delete_repo"),
		BodyBytes: 2000,
	}
	if got := e.Evaluate(p, base); got.Action != policy.ActionDeny {
		t.Errorf("all conditions satisfied: Action = %q, want %q", got.Action, policy.ActionDeny)
	}

	wrongAgent := base
	wrongAgent.AgentID = "agt_2"
	if got := e.Evaluate(p, wrongAgent); got.Action != policy.ActionAllow {
		t.Errorf("agent mismatch: Action = %q, want %q (default)", got.Action, policy.ActionAllow)
	}

	wrongTool := base
	wrongTool.MCPTool = strp("read_repo")
	if got := e.Evaluate(p, wrongTool); got.Action != policy.ActionAllow {
		t.Errorf("tool mismatch: Action = %q, want %q (default)", got.Action, policy.ActionAllow)
	}

	smallBody := base
	smallBody.BodyBytes = 10
	if got := e.Evaluate(p, smallBody); got.Action != policy.ActionAllow {
		t.Errorf("body mismatch: Action = %q, want %q (default)", got.Action, policy.ActionAllow)
	}
}

func TestBuiltinEvaluator_AllowWarnDenyExample(t *testing.T) {
	t.Parallel()

	// The policy shape from spec 0009 §Surface's authoring example.
	sizeLimit := int64(32768)
	rateLimit := 60
	p := &policy.Policy{
		Default: policy.ActionAllow,
		OnError: policy.ActionDeny,
		Rules: []policy.Rule{
			{Action: policy.ActionAllow, Match: policy.Match{Agents: []string{"agt_01J9X8"}, Tools: []string{"search_docs", "read_*"}}},
			{Action: policy.ActionDeny, Match: policy.Match{Tools: []string{"delete_*"}}},
			{Action: policy.ActionWarn, Match: policy.Match{MaxBodyBytes: &sizeLimit}},
			{Action: policy.ActionDeny, Match: policy.Match{RatePerMin: &rateLimit}},
		},
	}
	e := policy.NewBuiltinEvaluator()

	tests := []struct {
		name string
		ctx  policy.RequestContext
		want policy.Action
	}{
		{
			name: "allowed agent+tool",
			ctx:  policy.RequestContext{Protocol: policy.ProtocolMCP, AgentID: "agt_01J9X8", MCPTool: strp("read_file")},
			want: policy.ActionAllow,
		},
		{
			name: "denied tool",
			ctx:  policy.RequestContext{Protocol: policy.ProtocolMCP, AgentID: "agt_other", MCPTool: strp("delete_repo")},
			want: policy.ActionDeny,
		},
		{
			name: "warned large body",
			ctx:  policy.RequestContext{Protocol: policy.ProtocolMCP, AgentID: "agt_other", MCPTool: strp("noop"), BodyBytes: sizeLimit},
			want: policy.ActionWarn,
		},
		{
			name: "denied rate",
			ctx:  policy.RequestContext{Protocol: policy.ProtocolMCP, AgentID: "agt_other", MCPTool: strp("noop"), BodyBytes: 10, RatePerMin: rateLimit + 1},
			want: policy.ActionDeny,
		},
		{
			name: "no match falls to default allow",
			ctx:  policy.RequestContext{Protocol: policy.ProtocolMCP, AgentID: "agt_other", MCPTool: strp("noop"), BodyBytes: 10, RatePerMin: 1},
			want: policy.ActionAllow,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := e.Evaluate(p, tt.ctx)
			if got.Action != tt.want {
				t.Errorf("Action = %q, want %q", got.Action, tt.want)
			}
		})
	}
}

func TestBuiltinEvaluator_Deterministic(t *testing.T) {
	t.Parallel()

	limit := int64(100)
	p := &policy.Policy{
		Default: policy.ActionWarn,
		OnError: policy.ActionDeny,
		Rules: []policy.Rule{
			{Action: policy.ActionDeny, Match: policy.Match{Tools: []string{"delete_*"}, MaxBodyBytes: &limit}},
		},
	}
	ctx := policy.RequestContext{Protocol: policy.ProtocolMCP, AgentID: "agt_1", MCPTool: strp("delete_x"), BodyBytes: 200}
	e := policy.NewBuiltinEvaluator()

	first := e.Evaluate(p, ctx)
	for i := 0; i < 50; i++ {
		got := e.Evaluate(p, ctx)
		if got != first {
			t.Fatalf("run %d: Evaluate() = %+v, want %+v (must be deterministic)", i, got, first)
		}
	}
}
