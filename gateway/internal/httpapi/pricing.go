package httpapi

import (
	"fmt"
	"regexp"

	"github.com/zerkerlabs/farcaster/gateway/internal/agent"
)

// pricingNetwork and pricingAsset are the only network/asset v1 accepts for
// x402 metering (spec 0005 decisions 2, 9, 10): USDC on Base. Other values are
// rejected at write time rather than stored for a proxy surface that cannot
// verify them.
const (
	pricingNetwork = "base"
	pricingAsset   = "USDC"
)

// Precompiled validators for the pricing fields.
//
//   - pricePayToRe: a 20-byte hex Ethereum-style address (the operator's public
//     payout address; the gateway holds no key, Decision 9).
//   - priceAmountRe: a non-negative integer string in the asset's smallest unit
//     (e.g. USDC 6-decimals); amounts are strings, never floats.
var (
	pricePayToRe  = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)
	priceAmountRe = regexp.MustCompile(`^[0-9]+$`)
)

// pricingPayload is the JSON representation of an agent's pricing (spec 0005),
// used in both the create/update requests and the agent response. A nil
// *pricingPayload means the agent is unpriced.
type pricingPayload struct {
	Amount  string            `json:"amount"`
	Asset   string            `json:"asset"`
	Network string            `json:"network"`
	PayTo   string            `json:"pay_to"`
	Tools   map[string]string `json:"tools,omitempty"`
}

// toPricing maps the wire payload onto the domain type. A nil payload yields a
// nil *agent.Pricing (unpriced).
func (p *pricingPayload) toPricing() *agent.Pricing {
	if p == nil {
		return nil
	}
	out := &agent.Pricing{
		Amount:  p.Amount,
		Asset:   p.Asset,
		Network: p.Network,
		PayTo:   p.PayTo,
	}
	if len(p.Tools) > 0 {
		out.Tools = make(map[string]string, len(p.Tools))
		for k, v := range p.Tools {
			out.Tools[k] = v
		}
	}
	return out
}

// pricingToPayload maps the domain type onto the wire payload. A nil *Pricing
// yields a nil payload (serialised as JSON null / unpriced).
func pricingToPayload(p *agent.Pricing) *pricingPayload {
	if p == nil {
		return nil
	}
	out := &pricingPayload{
		Amount:  p.Amount,
		Asset:   p.Asset,
		Network: p.Network,
		PayTo:   p.PayTo,
	}
	if len(p.Tools) > 0 {
		out.Tools = make(map[string]string, len(p.Tools))
		for k, v := range p.Tools {
			out.Tools[k] = v
		}
	}
	return out
}

// validatePricing checks that p is a valid x402 metering configuration for an
// agent whose resulting protocol is protocol (spec 0005). A nil p means the
// agent is unpriced, which is always valid. Messages are deliberately coarse so
// no internal detail leaks (invariant #3, AGENTS.md).
func validatePricing(p *agent.Pricing, protocol string) error {
	if p == nil {
		return nil
	}
	if p.Network != pricingNetwork {
		return fmt.Errorf("pricing.network must be %q", pricingNetwork)
	}
	if p.Asset != pricingAsset {
		return fmt.Errorf("pricing.asset must be %q", pricingAsset)
	}
	if !pricePayToRe.MatchString(p.PayTo) {
		return fmt.Errorf("pricing.pay_to must be a hex address")
	}
	if !priceAmountRe.MatchString(p.Amount) {
		return fmt.Errorf("pricing.amount must be a non-negative integer string")
	}
	// Per-tool overrides only make sense for mcp agents; an http agent is
	// route-priced only (spec 0005 decision 5).
	if len(p.Tools) > 0 && protocol != "mcp" {
		return fmt.Errorf("per-tool pricing requires protocol=mcp")
	}
	for tool, amount := range p.Tools {
		if tool == "" {
			return fmt.Errorf("pricing.tools has an empty tool name")
		}
		if !priceAmountRe.MatchString(amount) {
			return fmt.Errorf("pricing.tools amount must be a non-negative integer string")
		}
	}
	return nil
}
