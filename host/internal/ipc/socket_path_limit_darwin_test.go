//go:build darwin

package ipc

import (
	"strings"
	"testing"
)

func TestValidatePairSocketPath_DarwinLimit(t *testing.T) {
	limit := darwinSocketPathLimit - 1
	if limit <= 0 {
		t.Fatalf("invalid darwin socket path limit: %d", limit)
	}

	validPath := "/" + strings.Repeat("a", limit-1)
	if err := validatePairSocketPath(validPath); err != nil {
		t.Fatalf("validatePairSocketPath() error: %v", err)
	}

	invalidPath := "/" + strings.Repeat("a", limit)
	if err := validatePairSocketPath(invalidPath); err == nil {
		t.Fatalf("validatePairSocketPath() expected error for long path")
	}
}
