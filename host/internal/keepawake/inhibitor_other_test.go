//go:build !darwin

package keepawake

import (
	"context"
	"testing"

	hostErrors "github.com/pseudocoder/host/internal/errors"
)

func TestOtherAdapterReturnsUnsupported(t *testing.T) {
	adapter := NewDefaultAdapter()
	_, err := adapter.Acquire(context.Background())
	if err == nil {
		t.Fatal("expected unsupported error")
	}
	if got := hostErrors.GetCode(err); got != hostErrors.CodeKeepAwakeUnsupportedEnvironment {
		t.Fatalf("code=%s want=%s", got, hostErrors.CodeKeepAwakeUnsupportedEnvironment)
	}
}
