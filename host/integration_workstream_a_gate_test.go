//go:build integration
// +build integration

package integration_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const (
	// Gate 1 KPI thresholds from docs/PLANS.md.
	workstreamAAttempts         = 10
	workstreamAMinSuccessRate   = 0.90
	workstreamAMaxMedianConnect = 3 * time.Minute
)

// TestIntegrationWorkstreamAOnboardingGateKPI automates the Gate 1 onboarding
// KPI checks with repeated fresh host onboarding attempts.
//
// Each attempt exercises:
// 1. Generate pairing code (/pair/generate)
// 2. Exchange code for token (/pair)
// 3. Connect via authenticated WebSocket and receive session.status
//
// The test then enforces:
// - first successful connect rate >= 90%
// - median time-to-first-connected-session <= 3 minutes
func TestIntegrationWorkstreamAOnboardingGateKPI(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping Workstream A gate KPI integration test in short mode")
	}

	durations := make([]time.Duration, 0, workstreamAAttempts)

	for attempt := 1; attempt <= workstreamAAttempts; attempt++ {
		addr := getFreeAddr(t)
		host := startHostWithAuth(t, addr, "sleep 30")

		duration, err := runOnboardingAttempt(addr, fmt.Sprintf("Gate1 Device %02d", attempt))
		host.stop(t)

		if err != nil {
			t.Logf("attempt %d failed: %v", attempt, err)
			continue
		}

		durations = append(durations, duration)
	}

	successes := len(durations)
	successRate := float64(successes) / float64(workstreamAAttempts)

	if successRate < workstreamAMinSuccessRate {
		t.Fatalf(
			"first successful connect rate %.2f%% below target %.2f%% (%d/%d successes)",
			successRate*100,
			workstreamAMinSuccessRate*100,
			successes,
			workstreamAAttempts,
		)
	}

	if successes == 0 {
		t.Fatal("all onboarding attempts failed; cannot compute median connect time")
	}

	median := medianDuration(durations)
	if median > workstreamAMaxMedianConnect {
		t.Fatalf(
			"median time-to-first-connected-session %s exceeds target %s",
			median,
			workstreamAMaxMedianConnect,
		)
	}

	t.Logf(
		"Gate 1 onboarding KPI probe: success_rate=%.2f%% (%d/%d), median_connect=%s",
		successRate*100,
		successes,
		workstreamAAttempts,
		median,
	)
}

// runOnboardingAttempt performs one end-to-end onboarding connection attempt
// against a fresh host instance and measures completion duration.
func runOnboardingAttempt(addr, deviceName string) (time.Duration, error) {
	started := time.Now()

	code, err := requestPairingCode(addr)
	if err != nil {
		return 0, err
	}

	token, err := exchangePairingCode(addr, code, deviceName)
	if err != nil {
		return 0, err
	}

	if err := connectWithToken(addr, token); err != nil {
		return 0, err
	}

	return time.Since(started), nil
}

func requestPairingCode(addr string) (string, error) {
	resp, err := http.Post(fmt.Sprintf("http://%s/pair/generate", addr), "", nil)
	if err != nil {
		return "", fmt.Errorf("generate code request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("generate code status %d: %s", resp.StatusCode, body)
	}

	var payload generateCodeResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", fmt.Errorf("decode generate code response: %w", err)
	}

	if len(payload.Code) != 6 {
		return "", fmt.Errorf("expected 6-digit pairing code, got %q", payload.Code)
	}

	return payload.Code, nil
}

func exchangePairingCode(addr, code, deviceName string) (string, error) {
	body, err := json.Marshal(pairRequest{
		Code:       code,
		DeviceName: deviceName,
	})
	if err != nil {
		return "", fmt.Errorf("marshal pair request: %w", err)
	}

	resp, err := http.Post(
		fmt.Sprintf("http://%s/pair", addr),
		"application/json",
		bytes.NewReader(body),
	)
	if err != nil {
		return "", fmt.Errorf("pair request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("pair status %d: %s", resp.StatusCode, raw)
	}

	var payload pairResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", fmt.Errorf("decode pair response: %w", err)
	}

	if payload.Token == "" {
		return "", fmt.Errorf("pair response missing token")
	}

	return payload.Token, nil
}

func connectWithToken(addr, token string) error {
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+token)

	conn, _, err := websocket.DefaultDialer.Dial(fmt.Sprintf("ws://%s/ws", addr), headers)
	if err != nil {
		return fmt.Errorf("websocket dial with token failed: %w", err)
	}
	defer conn.Close()

	first, err := readEnvelope(conn, 2*time.Second)
	if err != nil {
		return fmt.Errorf("read first websocket message: %w", err)
	}

	if first.Type != "session.status" {
		return fmt.Errorf("expected session.status, got %s", first.Type)
	}

	return nil
}

func medianDuration(values []time.Duration) time.Duration {
	if len(values) == 0 {
		return 0
	}

	sorted := append([]time.Duration(nil), values...)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i] < sorted[j]
	})

	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}
	return (sorted[mid-1] + sorted[mid]) / 2
}
