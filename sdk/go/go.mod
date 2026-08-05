module github.com/zerkerlabs/gateway/sdk/go

go 1.26.4

require github.com/zerkerlabs/gateway/x402types v0.0.0

// x402types is an in-repo workspace module (ADR-0010), resolved by path.
replace github.com/zerkerlabs/gateway/x402types => ../../x402types
