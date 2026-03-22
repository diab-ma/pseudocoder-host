// Package netutil provides shared network utility functions.
package netutil

import (
	"log"
	"net"
	"net/http"
	"strings"
)

// IsLoopbackRequest checks if the HTTP request originates from the local machine.
// Returns true for loopback addresses, Unix socket connections, and local interface IPs.
func IsLoopbackRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		if isUnixSocketRemoteAddr(r.RemoteAddr) {
			return true
		}
		log.Printf("netutil: failed to parse RemoteAddr %q: %v", r.RemoteAddr, err)
		return false
	}

	ip := net.ParseIP(host)
	if ip == nil {
		log.Printf("netutil: failed to parse IP from host %q", host)
		return false
	}

	if ip.IsLoopback() {
		return true
	}

	return isLocalInterfaceIP(ip)
}

func isUnixSocketRemoteAddr(remoteAddr string) bool {
	if remoteAddr == "" {
		return true
	}
	return strings.HasPrefix(remoteAddr, "/") || strings.HasPrefix(remoteAddr, "@")
}

func isLocalInterfaceIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip4 := ip.To4(); ip4 != nil {
		ip = ip4
	}

	ifaces, err := net.Interfaces()
	if err != nil {
		log.Printf("netutil: failed to list interfaces: %v", err)
		return false
	}

	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var localIP net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				localIP = v.IP
			case *net.IPAddr:
				localIP = v.IP
			}
			if localIP == nil {
				continue
			}
			if localIP4 := localIP.To4(); localIP4 != nil {
				localIP = localIP4
			}
			if localIP.Equal(ip) {
				return true
			}
		}
	}

	return false
}
