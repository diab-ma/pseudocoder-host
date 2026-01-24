// Package main provides CLI commands for the pseudocoder host.
// This file centralizes address selection for local CLI commands.
package main

import (
	"fmt"
	"io"
)

func resolveAddrCandidates(addr string, port int, explicitPort bool, stderr io.Writer) []string {
	if addr != "" {
		if explicitPort {
			fmt.Fprintf(stderr, "Warning: --addr overrides --port; using %s\n", addr)
		}
		return []string{addr}
	}

	return defaultAddrCandidates(port)
}

func defaultAddrCandidates(port int) []string {
	portStr := fmt.Sprintf("%d", port)
	addrs := []string{"127.0.0.1:" + portStr}
	if ip := GetTailscaleIP(); ip != "" {
		addrs = append(addrs, ip+":"+portStr)
	}
	if ip := GetPreferredOutboundIP(); ip != "" {
		addrs = append(addrs, ip+":"+portStr)
	}
	return addrs
}
