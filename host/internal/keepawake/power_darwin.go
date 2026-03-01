//go:build darwin

package keepawake

import (
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

var batteryPercentRe = regexp.MustCompile(`(\d+)%`)

type darwinPowerProvider struct{}

// NewDefaultPowerProvider returns a darwin-specific power provider
// that parses pmset -g batt output.
func NewDefaultPowerProvider() PowerProvider {
	return &darwinPowerProvider{}
}

func (d *darwinPowerProvider) Snapshot() PowerSnapshot {
	out, err := exec.Command("pmset", "-g", "batt").Output()
	if err != nil {
		return PowerSnapshot{}
	}
	return parsePmsetOutput(string(out))
}

func parsePmsetOutput(output string) PowerSnapshot {
	snap := PowerSnapshot{}

	if strings.Contains(output, "'Battery Power'") {
		t := true
		f := false
		snap.OnBattery = &t
		snap.ExternalPower = &f
	} else if strings.Contains(output, "'AC Power'") {
		f := false
		t := true
		snap.OnBattery = &f
		snap.ExternalPower = &t
	}

	if m := batteryPercentRe.FindStringSubmatch(output); len(m) >= 2 {
		if pct, err := strconv.Atoi(m[1]); err == nil && pct >= 0 && pct <= 100 {
			snap.BatteryPercent = &pct
		}
	}

	return snap
}
