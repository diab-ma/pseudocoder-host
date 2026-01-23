//go:build !darwin

package ipc

func validatePairSocketPath(path string) error {
	return nil
}
