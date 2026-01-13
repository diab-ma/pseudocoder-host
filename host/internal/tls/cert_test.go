package tls

import (
	"crypto/tls"
	"crypto/x509"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGenerateCertificate(t *testing.T) {
	// Create a temporary directory for the test
	tmpDir, err := os.MkdirTemp("", "tls-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	certPath := filepath.Join(tmpDir, "test.crt")
	keyPath := filepath.Join(tmpDir, "test.key")

	// Generate a certificate
	info, err := GenerateCertificate(CertConfig{
		CertPath:      certPath,
		KeyPath:       keyPath,
		Hosts:         []string{"localhost", "127.0.0.1", "example.com"},
		ValidDuration: 24 * time.Hour,
		Organization:  "test-org",
	})
	if err != nil {
		t.Fatalf("GenerateCertificate failed: %v", err)
	}

	// Verify CertInfo fields
	if info.CertPath != certPath {
		t.Errorf("CertPath mismatch: got %s, want %s", info.CertPath, certPath)
	}
	if info.KeyPath != keyPath {
		t.Errorf("KeyPath mismatch: got %s, want %s", info.KeyPath, keyPath)
	}
	if !info.IsGenerated {
		t.Error("IsGenerated should be true for newly generated cert")
	}

	// Verify fingerprint format (colon-separated hex)
	if info.Fingerprint == "" {
		t.Error("Fingerprint should not be empty")
	}
	parts := strings.Split(info.Fingerprint, ":")
	if len(parts) != 32 { // SHA-256 = 32 bytes = 32 hex pairs
		t.Errorf("Fingerprint should have 32 parts, got %d", len(parts))
	}
	for _, part := range parts {
		if len(part) != 2 {
			t.Errorf("Each fingerprint part should be 2 chars, got %q", part)
		}
	}

	// Verify validity dates
	if info.NotBefore.After(time.Now()) {
		t.Error("NotBefore should not be in the future")
	}
	expectedExpiry := info.NotBefore.Add(24 * time.Hour)
	if info.NotAfter.Before(expectedExpiry.Add(-time.Minute)) || info.NotAfter.After(expectedExpiry.Add(time.Minute)) {
		t.Errorf("NotAfter should be ~24 hours after NotBefore")
	}

	// Verify files were created
	if _, err := os.Stat(certPath); os.IsNotExist(err) {
		t.Error("Certificate file was not created")
	}
	if _, err := os.Stat(keyPath); os.IsNotExist(err) {
		t.Error("Key file was not created")
	}

	// Verify key file permissions (should be 0600)
	keyInfo, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("Failed to stat key file: %v", err)
	}
	if keyInfo.Mode().Perm() != 0600 {
		t.Errorf("Key file permissions should be 0600, got %o", keyInfo.Mode().Perm())
	}

	// Verify the cert/key pair can be loaded
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatalf("Failed to load generated cert/key pair: %v", err)
	}

	// Parse and verify the certificate
	x509Cert, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("Failed to parse certificate: %v", err)
	}

	// Check organization
	if len(x509Cert.Subject.Organization) == 0 || x509Cert.Subject.Organization[0] != "test-org" {
		t.Errorf("Organization mismatch: got %v, want [test-org]", x509Cert.Subject.Organization)
	}

	// Check DNS names
	if len(x509Cert.DNSNames) < 2 {
		t.Errorf("Expected at least 2 DNS names, got %v", x509Cert.DNSNames)
	}
	hasLocalhost := false
	hasExample := false
	for _, name := range x509Cert.DNSNames {
		if name == "localhost" {
			hasLocalhost = true
		}
		if name == "example.com" {
			hasExample = true
		}
	}
	if !hasLocalhost {
		t.Error("DNS names should include localhost")
	}
	if !hasExample {
		t.Error("DNS names should include example.com")
	}

	// Check IP addresses
	hasIP := false
	for _, ip := range x509Cert.IPAddresses {
		if ip.String() == "127.0.0.1" {
			hasIP = true
			break
		}
	}
	if !hasIP {
		t.Error("IP addresses should include 127.0.0.1")
	}
}

func TestGenerateCertificateDefaults(t *testing.T) {
	// Create a temporary directory
	tmpDir, err := os.MkdirTemp("", "tls-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	certPath := filepath.Join(tmpDir, "default.crt")
	keyPath := filepath.Join(tmpDir, "default.key")

	// Generate with defaults (empty config except paths)
	info, err := GenerateCertificate(CertConfig{
		CertPath: certPath,
		KeyPath:  keyPath,
	})
	if err != nil {
		t.Fatalf("GenerateCertificate with defaults failed: %v", err)
	}

	// Load and verify
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatalf("Failed to load cert: %v", err)
	}

	x509Cert, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("Failed to parse cert: %v", err)
	}

	// Check default organization
	if len(x509Cert.Subject.Organization) == 0 || x509Cert.Subject.Organization[0] != "pseudocoder" {
		t.Errorf("Default organization should be 'pseudocoder', got %v", x509Cert.Subject.Organization)
	}

	// Check default validity (should be ~365 days)
	validity := info.NotAfter.Sub(info.NotBefore)
	expectedDays := 365
	actualDays := int(validity.Hours() / 24)
	if actualDays < expectedDays-1 || actualDays > expectedDays+1 {
		t.Errorf("Default validity should be ~%d days, got %d", expectedDays, actualDays)
	}
}

func TestLoadCertificate(t *testing.T) {
	// Create a temporary directory
	tmpDir, err := os.MkdirTemp("", "tls-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	certPath := filepath.Join(tmpDir, "load.crt")
	keyPath := filepath.Join(tmpDir, "load.key")

	// Generate a certificate first
	genInfo, err := GenerateCertificate(CertConfig{
		CertPath: certPath,
		KeyPath:  keyPath,
	})
	if err != nil {
		t.Fatalf("GenerateCertificate failed: %v", err)
	}

	// Load the certificate
	loadInfo, err := LoadCertificate(certPath, keyPath)
	if err != nil {
		t.Fatalf("LoadCertificate failed: %v", err)
	}

	// Verify loaded info matches generated info
	if loadInfo.CertPath != certPath {
		t.Errorf("CertPath mismatch")
	}
	if loadInfo.KeyPath != keyPath {
		t.Errorf("KeyPath mismatch")
	}
	if loadInfo.Fingerprint != genInfo.Fingerprint {
		t.Errorf("Fingerprint mismatch: got %s, want %s", loadInfo.Fingerprint, genInfo.Fingerprint)
	}
	if loadInfo.IsGenerated {
		t.Error("IsGenerated should be false for loaded cert")
	}
}

func TestLoadCertificateNotFound(t *testing.T) {
	_, err := LoadCertificate("/nonexistent/path.crt", "/nonexistent/path.key")
	if err == nil {
		t.Error("LoadCertificate should fail for nonexistent files")
	}
}

func TestEnsureCertificateGenerates(t *testing.T) {
	// Create a temporary directory
	tmpDir, err := os.MkdirTemp("", "tls-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	certPath := filepath.Join(tmpDir, "ensure.crt")
	keyPath := filepath.Join(tmpDir, "ensure.key")

	// EnsureCertificate should generate when files don't exist
	info, err := EnsureCertificate(CertConfig{
		CertPath: certPath,
		KeyPath:  keyPath,
	})
	if err != nil {
		t.Fatalf("EnsureCertificate failed: %v", err)
	}

	if !info.IsGenerated {
		t.Error("IsGenerated should be true when files didn't exist")
	}

	// Verify files exist now
	if _, err := os.Stat(certPath); os.IsNotExist(err) {
		t.Error("Certificate file should have been created")
	}
}

func TestEnsureCertificateLoads(t *testing.T) {
	// Create a temporary directory
	tmpDir, err := os.MkdirTemp("", "tls-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	certPath := filepath.Join(tmpDir, "existing.crt")
	keyPath := filepath.Join(tmpDir, "existing.key")

	// Generate first
	genInfo, err := GenerateCertificate(CertConfig{
		CertPath: certPath,
		KeyPath:  keyPath,
	})
	if err != nil {
		t.Fatalf("GenerateCertificate failed: %v", err)
	}

	// EnsureCertificate should load when files exist
	loadInfo, err := EnsureCertificate(CertConfig{
		CertPath: certPath,
		KeyPath:  keyPath,
	})
	if err != nil {
		t.Fatalf("EnsureCertificate failed: %v", err)
	}

	if loadInfo.IsGenerated {
		t.Error("IsGenerated should be false when files already existed")
	}

	if loadInfo.Fingerprint != genInfo.Fingerprint {
		t.Error("Fingerprint should match the original certificate")
	}
}

func TestEnsureCertificateGeneratesIfOnlyOneMissing(t *testing.T) {
	// Create a temporary directory
	tmpDir, err := os.MkdirTemp("", "tls-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	certPath := filepath.Join(tmpDir, "partial.crt")
	keyPath := filepath.Join(tmpDir, "partial.key")

	// Create only the cert file (simulate partial state)
	if err := os.WriteFile(certPath, []byte("dummy"), 0644); err != nil {
		t.Fatalf("Failed to create dummy cert: %v", err)
	}

	// EnsureCertificate should regenerate when only one file exists
	info, err := EnsureCertificate(CertConfig{
		CertPath: certPath,
		KeyPath:  keyPath,
	})
	if err != nil {
		t.Fatalf("EnsureCertificate failed: %v", err)
	}

	if !info.IsGenerated {
		t.Error("IsGenerated should be true when regenerating")
	}

	// Verify both files now exist and are valid
	_, err = tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Errorf("Generated cert/key pair should be valid: %v", err)
	}
}

func TestComputeFingerprint(t *testing.T) {
	// Create a temporary directory
	tmpDir, err := os.MkdirTemp("", "tls-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	certPath := filepath.Join(tmpDir, "fp.crt")
	keyPath := filepath.Join(tmpDir, "fp.key")

	// Generate a certificate
	_, err = GenerateCertificate(CertConfig{
		CertPath: certPath,
		KeyPath:  keyPath,
	})
	if err != nil {
		t.Fatalf("GenerateCertificate failed: %v", err)
	}

	// Load and compute fingerprint
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatalf("Failed to load cert: %v", err)
	}

	x509Cert, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("Failed to parse cert: %v", err)
	}

	fp := ComputeFingerprint(x509Cert)

	// Verify format
	if fp == "" {
		t.Error("Fingerprint should not be empty")
	}

	// Should be uppercase hex
	if strings.ToUpper(fp) != fp {
		t.Error("Fingerprint should be uppercase")
	}

	// Should have colons
	if !strings.Contains(fp, ":") {
		t.Error("Fingerprint should contain colons")
	}

	// Computing again should give the same result
	fp2 := ComputeFingerprint(x509Cert)
	if fp != fp2 {
		t.Error("Fingerprint should be deterministic")
	}
}

func TestLoadTLSConfig(t *testing.T) {
	// Create a temporary directory
	tmpDir, err := os.MkdirTemp("", "tls-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	certPath := filepath.Join(tmpDir, "cfg.crt")
	keyPath := filepath.Join(tmpDir, "cfg.key")

	// Generate a certificate
	_, err = GenerateCertificate(CertConfig{
		CertPath: certPath,
		KeyPath:  keyPath,
	})
	if err != nil {
		t.Fatalf("GenerateCertificate failed: %v", err)
	}

	// Load TLS config
	tlsCfg, err := LoadTLSConfig(certPath, keyPath)
	if err != nil {
		t.Fatalf("LoadTLSConfig failed: %v", err)
	}

	// Verify config
	if len(tlsCfg.Certificates) != 1 {
		t.Errorf("Expected 1 certificate, got %d", len(tlsCfg.Certificates))
	}

	if tlsCfg.MinVersion != tls.VersionTLS12 {
		t.Error("MinVersion should be TLS 1.2")
	}
}

func TestLoadTLSConfigInvalidPath(t *testing.T) {
	_, err := LoadTLSConfig("/nonexistent/cert.crt", "/nonexistent/key.key")
	if err == nil {
		t.Error("LoadTLSConfig should fail for nonexistent files")
	}
}

func TestDefaultPaths(t *testing.T) {
	// Test that default paths work
	certPath, err := DefaultCertPath()
	if err != nil {
		t.Fatalf("DefaultCertPath failed: %v", err)
	}
	if certPath == "" {
		t.Error("DefaultCertPath should not be empty")
	}
	if !strings.Contains(certPath, ".pseudocoder") {
		t.Error("DefaultCertPath should contain .pseudocoder")
	}
	if !strings.HasSuffix(certPath, "host.crt") {
		t.Error("DefaultCertPath should end with host.crt")
	}

	keyPath, err := DefaultKeyPath()
	if err != nil {
		t.Fatalf("DefaultKeyPath failed: %v", err)
	}
	if keyPath == "" {
		t.Error("DefaultKeyPath should not be empty")
	}
	if !strings.HasSuffix(keyPath, "host.key") {
		t.Error("DefaultKeyPath should end with host.key")
	}
}

func TestGenerateCertificateCreatesDirectory(t *testing.T) {
	// Create a temporary directory
	tmpDir, err := os.MkdirTemp("", "tls-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Use a nested path that doesn't exist
	nestedDir := filepath.Join(tmpDir, "nested", "certs")
	certPath := filepath.Join(nestedDir, "test.crt")
	keyPath := filepath.Join(nestedDir, "test.key")

	// GenerateCertificate should create the directory
	_, err = GenerateCertificate(CertConfig{
		CertPath: certPath,
		KeyPath:  keyPath,
	})
	if err != nil {
		t.Fatalf("GenerateCertificate failed: %v", err)
	}

	// Verify directory was created
	if _, err := os.Stat(nestedDir); os.IsNotExist(err) {
		t.Error("Nested directory should have been created")
	}
}
