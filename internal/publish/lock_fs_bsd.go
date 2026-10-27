//go:build darwin || freebsd

package publish

import (
	"fmt"
	"strings"
	"syscall"
)

// lockableFS refuses a directory on a network or user-space file system,
// where a file lock that succeeds may not exclude another host (NFS with
// local locks, SMB, WebDAV, FUSE).
func lockableFS(dir string) error {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return fmt.Errorf("cannot tell the file system of %s: %w", dir, err)
	}
	var b strings.Builder
	for _, c := range st.Fstypename {
		if c == 0 {
			break
		}
		b.WriteByte(byte(c)) // #nosec G115 -- a C char of the type name, reinterpreted as a byte
	}
	if name := b.String(); remoteFSName(name) {
		return fmt.Errorf("%s is on a %s file system, where a file lock may hold only on this host", dir, name)
	}
	return nil
}
