//go:build !windows

package build

import (
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// checkGrandchildKilled verifies that a process started by the command was
// killed with its process group.
func checkGrandchildKilled(t *testing.T, pidFile string) {
	t.Helper()
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("grandchild pid not recorded: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	// The orphaned grandchild is reaped by init; until then kill(pid, 0)
	// still succeeds on the zombie, so poll.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	syscall.Kill(pid, syscall.SIGKILL)
	t.Errorf("grandchild %d survived the timeout", pid)
}
