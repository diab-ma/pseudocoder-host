package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunFlags_NoArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runFlags(nil, &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stdout.String(), "Usage:") {
		t.Error("expected usage output")
	}
}

func TestRunFlags_UnknownCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runFlags([]string{"bogus"}, &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
}

func TestRunFlags_EnableMissingPhase(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runFlagsPhaseAction(nil, "enable", &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
}

func TestRunFlags_EnableInvalidPhase(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runFlagsPhaseAction([]string{"abc"}, "enable", &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
}

func TestRunFlags_KillSwitchInvalidValue(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runFlagsKillSwitch([]string{"maybe"}, &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
}

func TestRunFlags_RolloutStageMissing(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runFlagsRolloutStage(nil, &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
}
