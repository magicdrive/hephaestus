//go:build windows

package publish

import (
	"fmt"
	"syscall"
	"unsafe"
)

var (
	procGetVolumePathNameW = kernel32.NewProc("GetVolumePathNameW")
	procGetDriveTypeW      = kernel32.NewProc("GetDriveTypeW")
)

const driveRemote = 4 // DRIVE_REMOTE

// lockableFS refuses a directory on a network drive or share, where a
// LockFileEx lock depends on the server.
func lockableFS(dir string) error {
	p, err := syscall.UTF16PtrFromString(dir)
	if err != nil {
		return err
	}
	vol := make([]uint16, syscall.MAX_PATH+1)
	r, _, err := procGetVolumePathNameW.Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&vol[0])), uintptr(len(vol))) // #nosec G103 -- the Win32 call takes string pointers
	if r == 0 {
		return fmt.Errorf("cannot tell the volume of %s: %w", dir, err)
	}
	t, _, _ := procGetDriveTypeW.Call(uintptr(unsafe.Pointer(&vol[0]))) // #nosec G103 -- the Win32 call takes a string pointer
	if t == driveRemote {
		return fmt.Errorf("%s is on a network drive, where a file lock depends on the server", dir)
	}
	return nil
}
