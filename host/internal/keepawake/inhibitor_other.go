//go:build !darwin

package keepawake

import (
	"context"

	hostErrors "github.com/pseudocoder/host/internal/errors"
)

// NewDefaultAdapter returns a non-darwin adapter that safely degrades.
func NewDefaultAdapter() Adapter {
	return &unsupportedAdapter{}
}

type unsupportedAdapter struct{}

func (a *unsupportedAdapter) Acquire(ctx context.Context) (Handle, error) {
	return nil, hostErrors.New(hostErrors.CodeKeepAwakeUnsupportedEnvironment, "keep-awake is unsupported on this host")
}
