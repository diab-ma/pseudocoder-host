package errors

import (
	"strings"
	"testing"
)

const workstreamAMinActionabilityCoverage = 0.95

// TestWorkstreamAOnboardingActionabilityCoverage enforces the Gate 1
// onboarding actionability KPI over the high-frequency failure taxonomy set.
func TestWorkstreamAOnboardingActionabilityCoverage(t *testing.T) {
	onboardingFailureCodes := []string{
		// /pair endpoint failures.
		CodeAuthPairMethodNotAllowed,
		CodeAuthPairMissingCode,
		CodeAuthPairInvalidRequest,
		CodeAuthPairInvalidCode,
		CodeAuthPairExpiredCode,
		CodeAuthPairUsedCode,
		CodeAuthPairRateLimited,
		CodeAuthPairInternal,
		// /pair/generate endpoint failures.
		CodeAuthPairGenerateForbidden,
		CodeAuthPairGenerateMethodNotAllowed,
		CodeAuthPairGenerateInternal,
		// Connection taxonomy failures used by onboarding UX.
		CodeServerCertExpired,
		CodeServerCertMismatch,
		CodeServerCertUntrusted,
		CodeServerTimeout,
		CodeServerUnreachable,
	}

	var actionableCount int

	for _, code := range onboardingFailureCodes {
		action := strings.TrimSpace(GetNextAction(code))
		if action == "" {
			t.Errorf("missing next action for onboarding failure code %q", code)
			continue
		}
		actionableCount++
	}

	coverage := float64(actionableCount) / float64(len(onboardingFailureCodes))
	if coverage < workstreamAMinActionabilityCoverage {
		t.Fatalf(
			"onboarding actionability coverage %.2f%% below target %.2f%% (%d/%d)",
			coverage*100,
			workstreamAMinActionabilityCoverage*100,
			actionableCount,
			len(onboardingFailureCodes),
		)
	}
}
