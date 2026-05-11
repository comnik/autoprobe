package provider

import "os"

// envLookup is a tiny indirection so per-provider key fallback chains read
// cleanly. It returns the empty string if the variable is unset.
func envLookup(name string) string {
	return os.Getenv(name)
}
