// Package resource provides shared utilities for Rooms resource identifiers.
//
// Every resource type in Farcaster is identified by a prefixed opaque string of
// the form "<prefix>_<uuidv7>". This mirrors the convention the gateway module
// owns in gateway/internal/resource. Rooms does not import that package —
// modules do not share internal packages — so it keeps its own copy of the same
// generation logic rather than inventing a second scheme.
package resource

import (
	"fmt"

	"github.com/google/uuid"
)

// New generates a server-assigned resource ID of the form "<prefix>_<uuidv7>".
// Each resource type supplies a stable, per-type prefix (e.g. "rom" for rooms).
// Callers and clients must treat the result as an opaque string — never parse or
// construct IDs from parts.
func New(prefix string) (string, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return "", fmt.Errorf("generate resource id: %w", err)
	}
	return prefix + "_" + id.String(), nil
}
