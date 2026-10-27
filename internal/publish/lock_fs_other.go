//go:build !(linux || darwin || freebsd || windows)

package publish

// lockableFS cannot tell the file system type here; the lock itself still
// fails closed where the system cannot lock.
func lockableFS(string) error { return nil }
