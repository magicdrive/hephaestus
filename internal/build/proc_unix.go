//go:build !windows

package build

import (
	"os"
	"os/exec"
	"syscall"
)

// shellArgv runs a command string with the POSIX shell.
func shellArgv(script string) []string { return []string{"/bin/sh", "-c", script} }

// configureProcess starts the command in its own process group so that a
// timeout kills everything it spawned, not only the direct child.
func configureProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// The process group id equals the leader's pid (Setpgid with Pgid 0).
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if err == syscall.ESRCH {
			return os.ErrProcessDone
		}
		return err
	}
}

// envKey normalizes an environment variable name for comparison.
func envKey(k string) string { return k }

// findExecutable reports whether p is a regular file with an execute bit.
func findExecutable(p string, _ []string) (string, bool) {
	st, err := os.Stat(p)
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0o111 == 0 {
		return "", false
	}
	return p, true
}
