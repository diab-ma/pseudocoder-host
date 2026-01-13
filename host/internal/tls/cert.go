// Package tls provides TLS certificate generation and management for the host.
// It handles self-signed certificate creation for secure WebSocket connections
// and provides utilities for computing certificate fingerprints.
package tls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// CertConfig holds configuration for certificate generation.
type CertConfig struct {
	// CertPath is the path to write the certificate file.
	// If empty, defaults to ~/.pseudocoder/certs/host.crt
	CertPath string

	// KeyPath is the path to write the private key file.
	// If empty, defaults to ~/.pseudocoder/certs/host.key
	KeyPath string

	// Hosts is a list of hostnames and IP addresses for the certificate.
	// If empty, defaults to localhost and 127.0.0.1.
	Hosts []string

	// ValidDuration is how long the certificate should be valid.
	// If zero, defaults to 365 days.
	ValidDuration time.Duration

	// Organization is the organization name in the certificate subject.
	// If empty, defaults to "pseudocoder".
	Organization string
}

// CertInfo contains information about a loaded or generated certificate.
type CertInfo struct {
	// CertPath is the path to the certificate file.
	CertPath string

	// KeyPath is the path to the private key file.
	KeyPath string

	// Fingerprint is the SHA-256 fingerprint of the certificate.
	// Format: colon-separated hex bytes (e.g., "AA:BB:CC:...")
	Fingerprint string

	// NotBefore is when the certificate becomes valid.
	NotBefore time.Time

	// NotAfter is when the certificate expires.
	NotAfter time.Time

	// IsGenerated indicates whether the certificate was just generated.
	// False if it was loaded from existing files.
	IsGenerated bool
}

// DefaultCertPath returns the default certificate path.
// This is ~/.pseudocoder/certs/host.crt
func DefaultCertPath() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}
	return filepath.Join(homeDir, ".pseudocoder", "certs", "host.crt"), nil
}

// DefaultKeyPath returns the default private key path.
// This is ~/.pseudocoder/certs/host.key
func DefaultKeyPath() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}
	return filepath.Join(homeDir, ".pseudocoder", "certs", "host.key"), nil
}

// EnsureCertificate loads an existing certificate or generates a new one.
// If certPath and keyPath are provided and both files exist, loads them.
// If either file is missing, generates a new self-signed certificate.
// Returns information about the certificate including its fingerprint.
func EnsureCertificate(cfg CertConfig) (*CertInfo, error) {
	// Apply defaults
	certPath := cfg.CertPath
	keyPath := cfg.KeyPath
	if certPath == "" {
		var err error
		certPath, err = DefaultCertPath()
		if err != nil {
			return nil, err
		}
	}
	if keyPath == "" {
		var err error
		keyPath, err = DefaultKeyPath()
		if err != nil {
			return nil, err
		}
	}

	// Check if both files exist
	certExists := fileExists(certPath)
	keyExists := fileExists(keyPath)

	var info *CertInfo
	var err error

	if certExists && keyExists {
		// Load existing certificate
		info, err = LoadCertificate(certPath, keyPath)
		if err != nil {
			return nil, fmt.Errorf("failed to load certificate: %w", err)
		}
	} else {
		// Generate new certificate
		genCfg := cfg
		genCfg.CertPath = certPath
		genCfg.KeyPath = keyPath
		info, err = GenerateCertificate(genCfg)
		if err != nil {
			return nil, fmt.Errorf("failed to generate certificate: %w", err)
		}
	}

	return info, nil
}

// LoadCertificate loads an existing certificate and computes its fingerprint.
func LoadCertificate(certPath, keyPath string) (*CertInfo, error) {
	// Verify we can load the cert/key pair
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load certificate pair: %w", err)
	}

	// Parse the certificate to get details
	x509Cert, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("failed to parse certificate: %w", err)
	}

	return &CertInfo{
		CertPath:    certPath,
		KeyPath:     keyPath,
		Fingerprint: ComputeFingerprint(x509Cert),
		NotBefore:   x509Cert.NotBefore,
		NotAfter:    x509Cert.NotAfter,
		IsGenerated: false,
	}, nil
}

// GenerateCertificate creates a new self-signed certificate.
// The certificate and key are written to the specified paths.
func GenerateCertificate(cfg CertConfig) (*CertInfo, error) {
	// Apply defaults
	hosts := cfg.Hosts
	if len(hosts) == 0 {
		hosts = []string{"localhost", "127.0.0.1"}
	}

	validDuration := cfg.ValidDuration
	if validDuration == 0 {
		validDuration = 365 * 24 * time.Hour // 1 year
	}

	organization := cfg.Organization
	if organization == "" {
		organization = "pseudocoder"
	}

	// Generate ECDSA private key (P-256 curve).
	// ECDSA with P-256 provides equivalent security to RSA 3072-bit keys
	// with much smaller key size and faster operations.
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("failed to generate private key: %w", err)
	}

	// Generate a random serial number.
	// Serial numbers must be unique for each certificate.
	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("failed to generate serial number: %w", err)
	}

	// Create the certificate template
	notBefore := time.Now()
	notAfter := notBefore.Add(validDuration)

	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{organization},
			CommonName:   "pseudocoder host",
		},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
	}

	// Add hosts as SANs (Subject Alternative Names).
	// This allows the certificate to be valid for multiple hostnames/IPs.
	for _, host := range hosts {
		if ip := net.ParseIP(host); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
		} else {
			template.DNSNames = append(template.DNSNames, host)
		}
	}

	// Create the self-signed certificate.
	// For a self-signed cert, the issuer is the same as the subject.
	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &privateKey.PublicKey, privateKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create certificate: %w", err)
	}

	// Ensure the directory exists
	certDir := filepath.Dir(cfg.CertPath)
	if err := os.MkdirAll(certDir, 0700); err != nil {
		return nil, fmt.Errorf("failed to create certificate directory: %w", err)
	}

	// Write the certificate to file
	certFile, err := os.OpenFile(cfg.CertPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return nil, fmt.Errorf("failed to create certificate file: %w", err)
	}
	defer certFile.Close()

	if err := pem.Encode(certFile, &pem.Block{Type: "CERTIFICATE", Bytes: derBytes}); err != nil {
		return nil, fmt.Errorf("failed to write certificate: %w", err)
	}

	// Write the private key to file
	keyFile, err := os.OpenFile(cfg.KeyPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return nil, fmt.Errorf("failed to create key file: %w", err)
	}
	defer keyFile.Close()

	// Marshal the private key to PKCS#8 format
	keyBytes, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal private key: %w", err)
	}

	if err := pem.Encode(keyFile, &pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes}); err != nil {
		return nil, fmt.Errorf("failed to write private key: %w", err)
	}

	// Parse the certificate to compute fingerprint
	x509Cert, err := x509.ParseCertificate(derBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse generated certificate: %w", err)
	}

	return &CertInfo{
		CertPath:    cfg.CertPath,
		KeyPath:     cfg.KeyPath,
		Fingerprint: ComputeFingerprint(x509Cert),
		NotBefore:   notBefore,
		NotAfter:    notAfter,
		IsGenerated: true,
	}, nil
}

// ComputeFingerprint computes the SHA-256 fingerprint of a certificate.
// Returns the fingerprint as colon-separated uppercase hex bytes.
// Example: "AA:BB:CC:DD:EE:FF:..."
func ComputeFingerprint(cert *x509.Certificate) string {
	hash := sha256.Sum256(cert.Raw)
	hexStr := hex.EncodeToString(hash[:])

	// Convert to uppercase and add colons
	var parts []string
	for i := 0; i < len(hexStr); i += 2 {
		parts = append(parts, strings.ToUpper(hexStr[i:i+2]))
	}
	return strings.Join(parts, ":")
}

// ComputeFingerprintFromPEM computes the SHA-256 fingerprint from PEM-encoded certificate data.
// This is useful when you have a certificate file but not a parsed x509.Certificate.
func ComputeFingerprintFromPEM(pemData []byte) (string, error) {
	block, _ := pem.Decode(pemData)
	if block == nil {
		return "", fmt.Errorf("failed to decode PEM block")
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("failed to parse certificate: %w", err)
	}

	return ComputeFingerprint(cert), nil
}

// LoadTLSConfig loads a TLS configuration from certificate files.
// This is used to configure the HTTPS server.
func LoadTLSConfig(certPath, keyPath string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load certificate pair: %w", err)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
		// Prefer cipher suites that support forward secrecy
		CipherSuites: []uint16{
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
		},
	}, nil
}

// fileExists checks if a file exists and is not a directory.
func fileExists(path string) bool {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return false
	}
	return err == nil && !info.IsDir()
}
