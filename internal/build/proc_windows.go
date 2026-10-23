//go:build windows

package build

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// shellArgv runs a command string with cmd.exe. /d skips AutoRun commands
// and /s makes cmd strip exactly the outer quotes added by configureProcess.
func shellArgv(script string) []string { return []string{"cmd.exe", "/d", "/s", "/c", script} }

const createNewProcessGroup = 0x00000200

// configureProcess passes a shell script to cmd.exe verbatim (Go's argument
// quoting is not what cmd.exe parses) and kills the whole process tree on
// timeout with taskkill /T, since Windows has no process groups that a
// signal can reach.
func configureProcess(cmd *exec.Cmd) {
	attr := &syscall.SysProcAttr{CreationFlags: createNewProcessGroup}
	if len(cmd.Args) == 5 && strings.EqualFold(cmd.Args[0], "cmd.exe") && cmd.Args[1] == "/d" && cmd.Args[2] == "/s" && cmd.Args[3] == "/c" {
		attr.CmdLine = `cmd.exe /d /s /c "` + cmd.Args[4] + `"`
	}
	cmd.SysProcAttr = attr
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		kill := exec.Command(taskkill(), "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
		_ = kill.Run()
		return cmd.Process.Kill()
	}
}

func taskkill() string {
	if root := os.Getenv("SystemRoot"); root != "" {
		return filepath.Join(root, "System32", "taskkill.exe")
	}
	return "taskkill.exe"
}

// envKey normalizes an environment variable name: Windows names are
// case-insensitive.
func envKey(k string) string { return strings.ToUpper(k) }

// findExecutable tries p and p with every PATHEXT extension.
func findExecutable(p string, env []string) (string, bool) {
	exts := []string{".com", ".exe", ".bat", ".cmd"}
	if v, ok := lookupEnv(env, "PATHEXT"); ok && v != "" {
		exts = nil
		for _, e := range strings.Split(strings.ToLower(v), ";") {
			if e != "" {
				if e[0] != '.' {
					e = "." + e
				}
				exts = append(exts, e)
			}
		}
	}
	if filepath.Ext(p) != "" {
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			return p, true
		}
	}
	for _, e := range exts {
		if st, err := os.Stat(p + e); err == nil && st.Mode().IsRegular() {
			return p + e, true
		}
	}
	return "", false
}
