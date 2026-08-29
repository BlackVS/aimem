//go:build !linux

package server

// resources is Linux-only; other platforms report nothing.
func resources(string) map[string]any { return nil }
