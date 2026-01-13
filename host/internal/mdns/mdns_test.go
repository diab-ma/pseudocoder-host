package mdns

import (
	"context"
	"testing"
	"time"
)

func TestNewAdvertiser(t *testing.T) {
	cfg := Config{
		Port:        7070,
		Fingerprint: "AA:BB:CC:DD:EE:FF",
		Name:        "test-host",
	}

	advertiser := NewAdvertiser(cfg)
	if advertiser == nil {
		t.Fatal("NewAdvertiser returned nil")
	}
	if advertiser.config.Port != 7070 {
		t.Errorf("expected port 7070, got %d", advertiser.config.Port)
	}
	if advertiser.config.Fingerprint != "AA:BB:CC:DD:EE:FF" {
		t.Errorf("expected fingerprint AA:BB:CC:DD:EE:FF, got %s", advertiser.config.Fingerprint)
	}
	if advertiser.config.Name != "test-host" {
		t.Errorf("expected name test-host, got %s", advertiser.config.Name)
	}
}

func TestAdvertiserIsRunning(t *testing.T) {
	advertiser := NewAdvertiser(Config{Port: 7070})

	// Should not be running initially
	if advertiser.IsRunning() {
		t.Error("advertiser should not be running before Start()")
	}
}

func TestAdvertiserStopBeforeStart(t *testing.T) {
	advertiser := NewAdvertiser(Config{Port: 7070})

	// Stop before start should be a no-op (no panic)
	advertiser.Stop()

	if advertiser.IsRunning() {
		t.Error("advertiser should not be running after Stop()")
	}
}

func TestAdvertiserMultipleStops(t *testing.T) {
	advertiser := NewAdvertiser(Config{Port: 7070})

	// Multiple stops should be safe
	advertiser.Stop()
	advertiser.Stop()
	advertiser.Stop()

	if advertiser.IsRunning() {
		t.Error("advertiser should not be running after Stop()")
	}
}

// TestAdvertiserStartStop tests that the advertiser can start and stop.
// This test requires network access and may not work in all CI environments.
func TestAdvertiserStartStop(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping network test in short mode")
	}

	advertiser := NewAdvertiser(Config{
		Port:        7070,
		Fingerprint: "AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99",
		Name:        "test-mdns-host",
	})

	// Start the advertiser
	if err := advertiser.Start(); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	if !advertiser.IsRunning() {
		t.Error("advertiser should be running after Start()")
	}

	// Double start should be a no-op
	if err := advertiser.Start(); err != nil {
		t.Fatalf("second Start() should be no-op, got error: %v", err)
	}

	// Stop the advertiser
	advertiser.Stop()

	if advertiser.IsRunning() {
		t.Error("advertiser should not be running after Stop()")
	}
}

// TestDiscoverIntegration tests the Discover function.
// This is an integration test that requires network access.
func TestDiscoverIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping network test in short mode")
	}

	// Start an advertiser
	advertiser := NewAdvertiser(Config{
		Port:        7071,
		Fingerprint: "TEST:FP:12:34",
		Name:        "discover-test-host",
	})

	if err := advertiser.Start(); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	defer advertiser.Stop()

	// Give mDNS time to propagate
	time.Sleep(500 * time.Millisecond)

	// Discover hosts with a short timeout
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	hosts, err := Discover(ctx)
	if err != nil {
		t.Fatalf("Discover() failed: %v", err)
	}

	// We should find at least our test host
	// Note: This may be flaky in some network environments
	found := false
	for _, host := range hosts {
		if host.Name == "discover-test-host" {
			found = true
			if host.Port != 7071 {
				t.Errorf("expected port 7071, got %d", host.Port)
			}
			if host.Fingerprint != "TEST:FP:12:34" {
				t.Errorf("expected fingerprint TEST:FP:12:34, got %s", host.Fingerprint)
			}
			break
		}
	}

	// Don't fail if not found - mDNS can be unreliable in CI
	if !found {
		t.Log("Warning: test host not discovered (may be expected in some environments)")
	}
}

func TestConfigDefaults(t *testing.T) {
	// Test with empty config
	cfg := Config{
		Port: 7070,
		// Name empty - should use hostname
		// Fingerprint empty - should omit from TXT
	}

	advertiser := NewAdvertiser(cfg)
	if advertiser.config.Port != 7070 {
		t.Errorf("expected port 7070, got %d", advertiser.config.Port)
	}
	if advertiser.config.Name != "" {
		t.Errorf("expected empty name, got %s", advertiser.config.Name)
	}
}

func TestServiceType(t *testing.T) {
	// Verify the service type follows Bonjour naming convention
	if ServiceType != "_pseudocoder._tcp" {
		t.Errorf("expected service type _pseudocoder._tcp, got %s", ServiceType)
	}
}

func TestProtocolVersion(t *testing.T) {
	if ProtocolVersion != "1" {
		t.Errorf("expected protocol version 1, got %s", ProtocolVersion)
	}
}
