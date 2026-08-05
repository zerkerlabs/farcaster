package httpapi

import (
	"github.com/zerkerlabs/gateway/gateway/internal/agent"
	"github.com/zerkerlabs/gateway/x402types"
)

// x402Scheme pins the wire conformance target: the coinbase x402 "typical
// flow", `exact` scheme handshake (spec 0005, Decision 7). The x402 version is
// x402types.Version.
const x402Scheme = "exact"

// x402MaxTimeoutSeconds is the authorization window advertised on every
// challenge. v1 has no per-agent override for this value (spec 0005 Surface
// example uses the same constant).
const x402MaxTimeoutSeconds = 60

// x402USDCBaseAsset is the USDC token contract address on Base — the only
// network/asset pair v1 prices (spec 0005 decisions 2, 9, 10; pricing.go pins
// Pricing.Asset to the "USDC" symbol, but the x402 wire `asset` field is the
// token contract address).
const x402USDCBaseAsset = "0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913"

// x402Challenge is the response body for a 402 Payment Required on a priced
// route (spec 0005 Surface, Decision 7).
type x402Challenge struct {
	X402Version int                             `json:"x402Version"`
	Accepts     []x402types.PaymentRequirements `json:"accepts"`
}

// paymentRequirements builds the single x402types.PaymentRequirements entry for a
// call to a priced agent (spec 0005 Surface, Decision 7). tool is the mcp_tool
// parsed from the request body (surface-4 parse, reused here); when non-nil and
// it has a configured override in p.Tools, that override is used as
// maxAmountRequired instead of the flat route price (Decision 5). resource is
// the request path the requirements are scoped to.
//
// This is the single source of truth for the required payment terms: the 402
// challenge renders it (via buildX402Challenge) and verification compares an
// inbound X-PAYMENT against it. Deriving both from the same helper is what
// keeps the advertised challenge and the enforced parameters from drifting.
func paymentRequirements(p *agent.Pricing, tool *string, resource string) x402types.PaymentRequirements {
	amount := p.Amount
	if tool != nil {
		if override, ok := p.Tools[*tool]; ok {
			amount = override
		}
	}
	return x402types.PaymentRequirements{
		Scheme:            x402Scheme,
		Network:           p.Network,
		MaxAmountRequired: amount,
		Asset:             x402USDCBaseAsset,
		PayTo:             p.PayTo,
		Resource:          resource,
		MaxTimeoutSeconds: x402MaxTimeoutSeconds,
	}
}

// buildX402Challenge constructs the 402 payment-requirements body for a call to
// a priced agent (spec 0005 Surface, Decision 7). It wraps the same
// paymentRequirements value that verification checks against.
func buildX402Challenge(p *agent.Pricing, tool *string, resource string) x402Challenge {
	return x402Challenge{
		X402Version: x402types.Version,
		Accepts:     []x402types.PaymentRequirements{paymentRequirements(p, tool, resource)},
	}
}
