package publish

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The helper process modes of TestLockHelperProcess.
const (
	lockHelperEnv  = "HEPHAESTUS_TEST_LOCK_HELPER"
	lockHelperPath = "HEPHAESTUS_TEST_LOCK_PATH"
)

// TestLockHelperProcess is not a test: it is the process the lock tests
// start and kill. "hold" takes the lock and waits; "hold-spawn" also starts
// a child ("sleep") that outlives it, to show the lock is not inherited.
func TestLockHelperProcess(t *testing.T) {
	mode := os.Getenv(lockHelperEnv)
	if mode == "" {
		t.Skip("helper process for the lock tests")
	}
	if mode == "sleep" {
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	if _, err := lockFile(os.Getenv(lockHelperPath), nil); err != nil {
		os.Stdout.WriteString("error " + err.Error() + "\n")
		os.Exit(1)
	}
	child := 0
	if mode == "hold-spawn" {
		c := helperCmd("sleep", "")
		if err := c.Start(); err != nil {
			os.Stdout.WriteString("error " + err.Error() + "\n")
			os.Exit(1)
		}
		child = c.Process.Pid
	}
	os.Stdout.WriteString("locked " + strconv.Itoa(child) + "\n")
	time.Sleep(time.Minute)
	os.Exit(0)
}

func helperCmd(mode, path string) *exec.Cmd {
	c := exec.Command(os.Args[0], "-test.run=^TestLockHelperProcess$", "-test.count=1")
	c.Env = append(os.Environ(), lockHelperEnv+"="+mode, lockHelperPath+"="+path)
	return c
}

// startHolder starts a helper process that holds the lock at p and returns
// it once the lock is held, with the pid of the helper's own child (0 if
// none).
func startHolder(t *testing.T, mode, p string) (*exec.Cmd, int) {
	t.Helper()
	c := helperCmd(mode, p)
	out, err := c.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Process.Kill(); _ = c.Wait() })
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "locked ") {
		t.Fatalf("helper: %q %v", line, err)
	}
	child, _ := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "locked ")))
	if child != 0 {
		t.Cleanup(func() {
			if p, err := os.FindProcess(child); err == nil {
				_ = p.Kill()
				_, _ = p.Wait()
			}
		})
	}
	return c, child
}

// TestLockKilledOwner: a publish process killed with SIGKILL (no deferred
// unlock runs) does not leave a lock behind; while it lives its lock is
// refused, naming it.
func TestLockKilledOwner(t *testing.T) {
	for _, mode := range []string{"hold", "hold-spawn"} {
		t.Run(mode, func(t *testing.T) {
			p := t.TempDir()
			c, _ := startHolder(t, mode, p)

			_, err := lockFile(p, nil)
			if err == nil || !strings.Contains(err.Error(), "another publish is running") ||
				!strings.Contains(err.Error(), "pid "+strconv.Itoa(c.Process.Pid)) {
				t.Fatalf("live owner: err = %v", err)
			}

			if err := c.Process.Kill(); err != nil { // SIGKILL on Unix, TerminateProcess on Windows
				t.Fatal(err)
			}
			_ = c.Wait()
			// The "hold-spawn" holder's child is still running: it did not
			// inherit the lock.
			unlock, err := lockFile(p, nil)
			if err != nil {
				t.Fatalf("after the owner was killed: %v", err)
			}
			unlock()
		})
	}
}

// TestLockLeftoverFile: a lock file nobody holds, such as the one an older
// hephaestus (O_EXCL lock) left when it was killed, does not block.
func TestLockLeftoverFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, lockName)
	if err := os.WriteFile(p, []byte("pid 999999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	unlock, err := lockFile(dir, func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) })
	if err != nil {
		t.Fatalf("leftover lock file: %v", err)
	}
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), "pid: "+strconv.Itoa(os.Getpid())+"\n") || !strings.Contains(string(b), "started: 2026-01-02T03:04:05Z") {
		t.Errorf("lock diagnostics: %q", b)
	}
	if _, err := lockFile(dir, nil); err == nil || !strings.Contains(err.Error(), "pid "+strconv.Itoa(os.Getpid())) {
		t.Errorf("second lock in the same process: %v", err)
	}
	unlock()
	if b, err := os.ReadFile(p); err != nil || len(b) != 0 {
		t.Errorf("after unlock the file is kept and empty: %q %v", b, err)
	}
	unlock2, err := lockFile(dir, nil)
	if err != nil {
		t.Fatalf("relock: %v", err)
	}
	unlock2()
}

func TestLockHolder(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if got := lockHolder(root); got != "holder unknown" {
		t.Errorf("missing file: %q", got)
	}
	for content, want := range map[string]string{
		"pid: 42\nhost: build-1\nstarted: 2026-01-02T03:04:05Z\nboot_id: x\n": "pid 42 on build-1 since 2026-01-02T03:04:05Z",
		"pid: 42\n":     "pid 42",
		"":              "holder unknown",
		"pid 42\n":      "holder unknown",
		"pid: nope\n":   "holder unknown",
		"garbage\x00\n": "holder unknown",
	} {
		if err := os.WriteFile(filepath.Join(dir, lockName), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := lockHolder(root); got != want {
			t.Errorf("lockHolder(%q) = %q, want %q", content, got, want)
		}
	}
}
