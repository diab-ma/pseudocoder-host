package keepawake

import "testing"

func TestNewDefaultPowerProvider(t *testing.T) {
	p := NewDefaultPowerProvider()
	if p == nil {
		t.Fatal("NewDefaultPowerProvider returned nil")
	}
	_ = p.Snapshot()
}
