// Package mdns provides optional mDNS/Bonjour service advertisement.
//
// When enabled, the host advertises itself on the local network using
// DNS-SD (DNS Service Discovery), allowing mobile apps to discover it
// without manual IP entry. This is an opt-in feature for security.
//
// The mDNS advertisement includes:
//   - Service type: _pseudocoder._tcp
//   - TXT records with version, fingerprint, and hostname
//
// Discovery only reveals presence; pairing codes are still required.
package mdns

import (
	"context"
	"fmt"
	"os"
	"sync"

	"github.com/grandcat/zeroconf"
)

// ServiceType is the mDNS service type for pseudocoder hosts.
// Follows the standard Bonjour naming convention: _<service>._<protocol>
const ServiceType = "_pseudocoder._tcp"

// ProtocolVersion identifies the mDNS protocol version for compatibility.
const ProtocolVersion = "1"

// Config holds configuration for mDNS advertisement.
type Config struct {
	// Port is the server port to advertise (e.g., 7070).
	Port int

	// Fingerprint is the TLS certificate fingerprint for trust verification.
	// This allows mobile apps to verify the host before pairing.
	Fingerprint string

	// Name is a human-readable name for this host.
	// Defaults to the system hostname if empty.
	Name string
}

// Advertiser manages mDNS/DNS-SD service registration.
// It advertises the pseudocoder host on the local network so mobile
// apps can discover it without typing IP addresses.
type Advertiser struct {
	config Config
	server *zeroconf.Server
	mu     sync.Mutex
}

// NewAdvertiser creates a new mDNS advertiser with the given configuration.
func NewAdvertiser(cfg Config) *Advertiser {
	return &Advertiser{
		config: cfg,
	}
}

// Start begins advertising the service via mDNS.
// It registers the service with DNS-SD so it can be discovered by
// apps on the same local network.
//
// Start is safe to call multiple times; subsequent calls are no-ops
// if already running.
func (a *Advertiser) Start() error {
	a.mu.Lock()
	defer a.mu.Unlock()

	// Already running
	if a.server != nil {
		return nil
	}

	// Determine the service name (instance name)
	name := a.config.Name
	if name == "" {
		hostname, err := os.Hostname()
		if err != nil {
			name = "pseudocoder"
		} else {
			name = hostname
		}
	}

	// Build TXT records for service metadata.
	// These provide information to clients before they connect:
	// - version: Protocol version for compatibility checks
	// - fp: Certificate fingerprint for trust verification
	// - name: Human-readable host name
	//
	// Note: DNS TXT records support up to 255 bytes per string.
	// SHA-256 fingerprints are 95 chars (32 hex pairs + 31 colons),
	// well within the limit.
	txtRecords := []string{
		fmt.Sprintf("version=%s", ProtocolVersion),
		fmt.Sprintf("name=%s", name),
	}

	// Include fingerprint if available (full fingerprint for trust verification)
	if a.config.Fingerprint != "" {
		txtRecords = append(txtRecords, fmt.Sprintf("fp=%s", a.config.Fingerprint))
	}

	// Register the mDNS service.
	// The service type is "_pseudocoder._tcp" on the ".local" domain.
	// The port and TXT records are included in the advertisement.
	server, err := zeroconf.Register(
		name,        // Instance name (e.g., "MacBook-Pro")
		ServiceType, // Service type (e.g., "_pseudocoder._tcp")
		"local.",    // Domain
		a.config.Port,
		txtRecords,
		nil, // Network interfaces (nil = all)
	)
	if err != nil {
		return fmt.Errorf("mdns register: %w", err)
	}

	a.server = server
	return nil
}

// Stop stops the mDNS advertisement and unregisters the service.
// It is safe to call Stop multiple times or on an advertiser that
// was never started.
func (a *Advertiser) Stop() {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.server != nil {
		a.server.Shutdown()
		a.server = nil
	}
}

// IsRunning returns true if the advertiser is currently running.
func (a *Advertiser) IsRunning() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.server != nil
}

// DiscoveredHost represents a host found via mDNS discovery.
// This is used by clients (like the mobile app) to display available hosts.
type DiscoveredHost struct {
	// Name is the human-readable name of the host.
	Name string

	// Host is the IP address or hostname.
	Host string

	// Port is the server port.
	Port int

	// Fingerprint is the TLS certificate fingerprint (if provided).
	Fingerprint string

	// Version is the protocol version.
	Version string
}

// Discover searches for pseudocoder hosts on the local network.
// It returns discovered hosts within the given timeout duration.
// This function is primarily for testing; mobile apps use native NSD.
func Discover(ctx context.Context) ([]DiscoveredHost, error) {
	resolver, err := zeroconf.NewResolver(nil)
	if err != nil {
		return nil, fmt.Errorf("mdns resolver: %w", err)
	}

	var (
		hosts []DiscoveredHost
		mu    sync.Mutex
		wg    sync.WaitGroup
	)

	entries := make(chan *zeroconf.ServiceEntry)

	// Collect results in a goroutine
	wg.Add(1)
	go func() {
		defer wg.Done()
		for entry := range entries {
			host := DiscoveredHost{
				Name: entry.Instance,
				Port: entry.Port,
			}

			// Prefer IPv4 address
			if len(entry.AddrIPv4) > 0 {
				host.Host = entry.AddrIPv4[0].String()
			} else if len(entry.AddrIPv6) > 0 {
				host.Host = entry.AddrIPv6[0].String()
			}

			// Parse TXT records
			for _, txt := range entry.Text {
				switch {
				case len(txt) > 3 && txt[:3] == "fp=":
					host.Fingerprint = txt[3:]
				case len(txt) > 8 && txt[:8] == "version=":
					host.Version = txt[8:]
				case len(txt) > 5 && txt[:5] == "name=":
					host.Name = txt[5:]
				}
			}

			mu.Lock()
			hosts = append(hosts, host)
			mu.Unlock()
		}
	}()

	// Browse for services
	err = resolver.Browse(ctx, ServiceType, "local.", entries)
	if err != nil {
		return nil, fmt.Errorf("mdns browse: %w", err)
	}

	// Wait for context to complete
	<-ctx.Done()

	// The zeroconf library closes the entries channel when context is done,
	// so we just need to wait for our goroutine to finish processing
	wg.Wait()

	return hosts, nil
}
