//go:build linux

package publish

import (
	"fmt"
	"syscall"
)

// remoteFS are the statfs magic numbers of network and user-space file
// systems (linux/magic.h, fs/smb). On them a lock that succeeds may still
// exclude only this host: NFS mounted with nolock or local_lock=flock|all
// grants flock locally, FUSE and 9p (WSL's /mnt/c) depend on the server.
var remoteFS = map[uint32]string{
	0x6969:     "nfs",
	0x517b:     "smb",
	0xff534d42: "cifs",
	0xfe534d42: "smb2",
	0x65735546: "fuse",
	0x01021997: "9p",
	0x00c36400: "ceph",
	0x5346414f: "afs",
	0x6b414653: "afs",
	0x73757245: "coda",
	0x564c:     "ncp",
	0x0bd00bd0: "lustre",
}

// lockableFS refuses a directory on a network or user-space file system,
// where a file lock that succeeds may not exclude another host.
func lockableFS(dir string) error {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return fmt.Errorf("cannot tell the file system of %s: %w", dir, err)
	}
	// Type is a signed field whose width depends on the architecture; the
	// magic numbers are 32-bit values.
	if name, ok := remoteFS[uint32(st.Type)]; ok { // #nosec G115 -- a 32-bit magic number, truncation intended
		return fmt.Errorf("%s is on a %s file system, where a file lock may hold only on this host", dir, name)
	}
	return nil
}
