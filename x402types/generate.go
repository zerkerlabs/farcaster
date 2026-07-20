// Package x402types is the canonical x402 wire contract shared by the gateway
// and the facilitator, generated from openapi.yaml (ADR-0010, Decision 4).
//
// openapi.yaml is the source of truth. Regenerate the Go types with:
//
//	go generate ./...
//
// The x402 numeric fields (amounts, validity bounds) are string-encoded 256-bit
// integers on the wire; parsing and validation of those strings is the
// consuming service's responsibility, not this contract's.
package x402types

//go:generate go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen -config oapi-codegen.yaml openapi.yaml
