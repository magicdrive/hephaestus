//go:build windows

package publish

import (
	"errors"
	"os"
	"syscall"
	"unsafe"
)

var (
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx   = kernel32.NewProc("LockFileEx")
	procUnlockFileEx = kernel32.NewProc("UnlockFileEx")
)

const (
	lockfileFailImmediately = 0x00000001
	lockfileExclusiveLock   = 0x00000002
	errorLockViolation      = syscall.Errno(33) // ERROR_LOCK_VIOLATION
)

// lockRange is the one byte that is locked, far beyond the diagnostics at
// the start of the file: a Windows byte-range lock is mandatory, and
// locking the contents would keep another process from reading who holds
// the lock.
func lockRange() *syscall.Overlapped {
	return &syscall.Overlapped{Offset: 0, OffsetHigh: 0x40000000}
}

// tryLock takes an exclusive LockFileEx lock on f without waiting. The lock
// belongs to the handle; Windows releases it when the handle is closed,
// which includes the termination of the process.
func tryLock(f *os.File) error {
	r, _, err := procLockFileEx.Call(f.Fd(), lockfileExclusiveLock|lockfileFailImmediately, 0, 1, 0,
		uintptr(unsafe.Pointer(lockRange()))) // #nosec G103 -- the Win32 call takes an OVERLAPPED pointer
	if r != 0 {
		return nil
	}
	if errors.Is(err, errorLockViolation) {
		return errLockBusy
	}
	return err
}

func unlockFile(f *os.File) error {
	r, _, err := procUnlockFileEx.Call(f.Fd(), 0, 1, 0,
		uintptr(unsafe.Pointer(lockRange()))) // #nosec G103 -- the Win32 call takes an OVERLAPPED pointer
	if r != 0 {
		return nil
	}
	return err
}
