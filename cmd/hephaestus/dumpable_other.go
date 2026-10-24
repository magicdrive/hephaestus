//go:build !linux

package hephaestus

// disableDumpable is Linux-only (see dumpable_linux.go). On macOS, other
// processes of the same user can read the environment with ps -E.
func disableDumpable() {}
