// Package resource provides shared utilities for Farcaster resource identifiers.
//
// Every resource type in Farcaster is identified by a prefixed opaque string of
// the form "<prefix>_<uuidv7>" (ADR-0009). This package owns the generation
// logic so individual resource types do not hand-roll IDs.
package resource

import (
	"fmt"

	"github.com/google/uuid"
)

// New generates a server-assigned resource ID of the form "<prefix>_<uuidv7>".
// Each resource type supplies a stable, per-type prefix (e.g. "agt" for agents).
// Callers and clients must treat the result as an opaque string — never parse or
// construct IDs from parts.
func New(prefix string) (string, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return "", fmt.Errorf("generate resource id: %w", err)
	}
	return prefix + "_" + id.String(), nil
}
