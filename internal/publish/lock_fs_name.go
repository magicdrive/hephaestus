package publish

import "strings"

// remoteFSName reports whether a file system type name (statfs
// f_fstypename) is a network or user-space file system.
func remoteFSName(name string) bool {
	switch name {
	case "nfs", "smbfs", "cifs", "afpfs", "webdav", "ftp", "9p":
		return true
	}
	return strings.Contains(name, "fuse")
}
