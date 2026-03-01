//go:build darwin

package keepawake

import "testing"

func TestParsePmsetOutput_ACPower(t *testing.T) {
	output := `Now drawing from 'AC Power'
 -InternalBattery-0 (id=1234567)	85%; charged; 0:00 remaining present: true
`
	snap := parsePmsetOutput(output)

	if snap.OnBattery == nil || *snap.OnBattery {
		t.Error("OnBattery should be false for AC power")
	}
	if snap.ExternalPower == nil || !*snap.ExternalPower {
		t.Error("ExternalPower should be true for AC power")
	}
	if snap.BatteryPercent == nil || *snap.BatteryPercent != 85 {
		t.Errorf("BatteryPercent = %v, want 85", snap.BatteryPercent)
	}
}

func TestParsePmsetOutput_BatteryPower(t *testing.T) {
	output := `Now drawing from 'Battery Power'
 -InternalBattery-0 (id=1234567)	42%; discharging; 3:15 remaining present: true
`
	snap := parsePmsetOutput(output)

	if snap.OnBattery == nil || !*snap.OnBattery {
		t.Error("OnBattery should be true for battery power")
	}
	if snap.ExternalPower == nil || *snap.ExternalPower {
		t.Error("ExternalPower should be false for battery power")
	}
	if snap.BatteryPercent == nil || *snap.BatteryPercent != 42 {
		t.Errorf("BatteryPercent = %v, want 42", snap.BatteryPercent)
	}
}

func TestParsePmsetOutput_Empty(t *testing.T) {
	snap := parsePmsetOutput("")

	if snap.OnBattery != nil {
		t.Error("OnBattery should be nil for empty output")
	}
	if snap.ExternalPower != nil {
		t.Error("ExternalPower should be nil for empty output")
	}
	if snap.BatteryPercent != nil {
		t.Error("BatteryPercent should be nil for empty output")
	}
}
