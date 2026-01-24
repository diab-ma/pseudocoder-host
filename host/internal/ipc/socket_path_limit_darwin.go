//go:build darwin

package ipc

import (
	"fmt"

	"golang.org/x/sys/unix"
)

const darwinSocketPathLimit = len(unix.RawSockaddrUnix{}.Path)

func validatePairSocketPath(path string) error {
	if path == "" {
		return nil
	}
	limit := darwinSocketPathLimit - 1
	if len(path) > limit {
		return fmt.Errorf("pairing IPC socket path exceeds %d bytes: %s", limit, path)
	}
	return nil
}
