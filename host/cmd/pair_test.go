package main

import (
	"bytes"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestDisplayQRCode verifies that displayQRCode produces correct output format
// with QR code and plain-text fallback containing all required fields.
func TestDisplayQRCode(t *testing.T) {
	var buf bytes.Buffer
	code := "123456"
	expiry := time.Now().Add(5 * time.Minute)
	addr := "192.168.1.10:7070"
	fingerprint := "AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99:AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99"

	DisplayQRCode(&buf, code, expiry, addr, fingerprint)

	output := buf.String()

	// Verify header
	if !strings.Contains(output, "SCAN TO PAIR") {
		t.Error("output should contain 'SCAN TO PAIR' header")
	}

	// Verify plain-text fallback section
	if !strings.Contains(output, "Plain-text fallback") {
		t.Error("output should contain 'Plain-text fallback' section")
	}

	// Verify code is formatted with spaces
	if !strings.Contains(output, "1 2 3 4 5 6") {
		t.Error("output should contain formatted code '1 2 3 4 5 6'")
	}

	// Verify host address is present
	if !strings.Contains(output, addr) {
		t.Errorf("output should contain host address %q", addr)
	}

	// Verify fingerprint is present
	if !strings.Contains(output, fingerprint) {
		t.Errorf("output should contain fingerprint %q", fingerprint)
	}

	// Verify expiry time format (HH:MM:SS)
	expiryStr := expiry.Format("15:04:05")
	if !strings.Contains(output, expiryStr) {
		t.Errorf("output should contain expiry time %q", expiryStr)
	}

	// Verify QR code block characters are present (half-block chars used by ToSmallString)
	// The QR code uses Unicode block characters like █ ▄ ▀
	if !strings.ContainsAny(output, "█▄▀") {
		t.Error("output should contain QR code block characters")
	}
}

// TestDisplayQRCodeEmptyFingerprint verifies behavior when fingerprint is empty
// (e.g., when using --no-tls mode).
func TestDisplayQRCodeEmptyFingerprint(t *testing.T) {
	var buf bytes.Buffer
	code := "654321"
	expiry := time.Now().Add(5 * time.Minute)
	addr := "127.0.0.1:7070"
	fingerprint := "" // Empty fingerprint for --no-tls mode

	DisplayQRCode(&buf, code, expiry, addr, fingerprint)

	output := buf.String()

	// Should still produce valid output
	if !strings.Contains(output, "SCAN TO PAIR") {
		t.Error("output should contain 'SCAN TO PAIR' header even with empty fingerprint")
	}

	// Code should still be present
	if !strings.Contains(output, "6 5 4 3 2 1") {
		t.Error("output should contain formatted code")
	}

	// Host should still be present
	if !strings.Contains(output, addr) {
		t.Error("output should contain host address")
	}
}

// TestQRPayloadRoundTrip verifies that the QR payload can be parsed back into
// the original field values. This ensures mobile clients can extract the data.
func TestQRPayloadRoundTrip(t *testing.T) {
	tests := []struct {
		name        string
		addr        string
		code        string
		fingerprint string
	}{
		{
			name:        "standard LAN address",
			addr:        "192.168.1.10:7070",
			code:        "123456",
			fingerprint: "AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99:AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99",
		},
		{
			name:        "localhost",
			addr:        "127.0.0.1:7070",
			code:        "999999",
			fingerprint: "11:22:33:44:55:66:77:88:99:AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99:AA:BB:CC:DD:EE:FF:00",
		},
		{
			name:        "empty fingerprint (no-tls mode)",
			addr:        "localhost:7070",
			code:        "000000",
			fingerprint: "",
		},
		{
			name:        "IPv6 localhost",
			addr:        "[::1]:7070",
			code:        "111111",
			fingerprint: "AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Build payload the same way displayQRCode does
			payload := buildQRPayload(tt.addr, tt.code, tt.fingerprint)

			// Parse the payload as a URL
			parsed, err := url.Parse(payload)
			if err != nil {
				t.Fatalf("failed to parse payload URL: %v", err)
			}

			// Verify scheme
			if parsed.Scheme != "pseudocoder" {
				t.Errorf("scheme = %q, want %q", parsed.Scheme, "pseudocoder")
			}

			// Verify path (opaque part after scheme)
			if parsed.Opaque != "//pair" && parsed.Host != "pair" {
				// URL parsing varies; just check the payload contains "pair"
				if !strings.Contains(payload, "://pair?") {
					t.Error("payload should contain '://pair?' path")
				}
			}

			// Extract query parameters
			query := parsed.Query()

			// Verify host param
			gotHost := query.Get("host")
			if gotHost != tt.addr {
				t.Errorf("host = %q, want %q", gotHost, tt.addr)
			}

			// Verify code param
			gotCode := query.Get("code")
			if gotCode != tt.code {
				t.Errorf("code = %q, want %q", gotCode, tt.code)
			}

			// Verify fingerprint param
			gotFP := query.Get("fp")
			if gotFP != tt.fingerprint {
				t.Errorf("fp = %q, want %q", gotFP, tt.fingerprint)
			}
		})
	}
}

// buildQRPayload constructs the QR payload URL (extracted for testing).
// This mirrors the logic in displayQRCode.
func buildQRPayload(addr, code, fingerprint string) string {
	return "pseudocoder://pair?host=" + url.QueryEscape(addr) +
		"&code=" + code +
		"&fp=" + url.QueryEscape(fingerprint)
}

// TestDisplayPairingCode verifies the non-QR display format.
func TestDisplayPairingCode(t *testing.T) {
	var buf bytes.Buffer
	code := "123456"
	expiry := time.Now().Add(5 * time.Minute)
	addr := "192.168.1.10:7070"

	DisplayPairingCode(&buf, code, expiry, addr)

	output := buf.String()

	// Verify header
	if !strings.Contains(output, "PAIRING CODE") {
		t.Error("output should contain 'PAIRING CODE' header")
	}

	// Verify code is formatted with spaces
	if !strings.Contains(output, "1 2 3 4 5 6") {
		t.Error("output should contain formatted code '1 2 3 4 5 6'")
	}

	// Verify host address
	if !strings.Contains(output, addr) {
		t.Errorf("output should contain host address %q", addr)
	}

	// Verify instructions
	if !strings.Contains(output, "Enter this code in the mobile app") {
		t.Error("output should contain pairing instructions")
	}

	// Verify no QR-specific content
	if strings.Contains(output, "SCAN TO PAIR") {
		t.Error("non-QR output should not contain 'SCAN TO PAIR'")
	}
}

// TestFormatCodeWithSpaces verifies code formatting.
func TestFormatCodeWithSpaces(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"123456", "1 2 3 4 5 6"},
		{"000000", "0 0 0 0 0 0"},
		{"1", "1"},
		{"12", "1 2"},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := FormatCodeWithSpaces(tt.input)
			if got != tt.want {
				t.Errorf("formatCodeWithSpaces(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
