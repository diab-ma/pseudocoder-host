//go:build !darwin

package keepawake

type fallbackPowerProvider struct{}

// NewDefaultPowerProvider returns a fallback provider that reports
// all-unknown power state on non-darwin platforms.
func NewDefaultPowerProvider() PowerProvider {
	return &fallbackPowerProvider{}
}

func (f *fallbackPowerProvider) Snapshot() PowerSnapshot {
	return PowerSnapshot{}
}
