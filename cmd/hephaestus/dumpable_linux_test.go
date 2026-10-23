//go:build linux

package hephaestus

import (
	"syscall"
	"testing"
)

// prGetDumpable is PR_GET_DUMPABLE from <linux/prctl.h>.
const prGetDumpable = 3

func TestDisableDumpable(t *testing.T) {
	disableDumpable()
	r, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prGetDumpable, 0, 0)
	if errno != 0 || r != 0 {
		t.Fatalf("PR_GET_DUMPABLE = %d (errno %v), want 0", r, errno)
	}
}
