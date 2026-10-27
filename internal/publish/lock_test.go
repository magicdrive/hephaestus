package publish

import (
	"bufio"
	"errors"
	"io/fs"
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
// a child ("sleep") that outlives it, to show the lock is not inherited;
// "try" reports whether it got the lock and waits either way.
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
		if mode == "try" {
			time.Sleep(time.Minute)
		}
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
			l, err := lockFile(p, nil)
			if err != nil {
				t.Fatalf("after the owner was killed: %v", err)
			}
			l.unlock()
		})
	}
}

// TestLockLegacyFileRefused: the file of an older hephaestus (O_EXCL, "pid
// N", no file lock) cannot tell a running older publish from a killed one,
// so it is refused whatever the pid, and left as it was; once someone
// empties it, the lock is taken.
func TestLockLegacyFileRefused(t *testing.T) {
	for _, content := range []string{"pid 999999\n", "pid " + strconv.Itoa(os.Getpid()) + "\n", "pid 1"} {
		dir := t.TempDir()
		p := filepath.Join(dir, lockName)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := lockFile(dir, nil)
		if err == nil || !strings.Contains(err.Error(), "older hephaestus") || !strings.Contains(err.Error(), "do not remove it") {
			t.Fatalf("%q: err = %v", content, err)
		}
		if b, _ := os.ReadFile(p); string(b) != content {
			t.Errorf("%q: the file was changed to %q", content, b)
		}
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		l, err := lockFile(dir, func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) })
		if err != nil {
			t.Fatalf("emptied: %v", err)
		}
		b, _ := os.ReadFile(p)
		if !strings.Contains(string(b), "pid: "+strconv.Itoa(os.Getpid())+"\n") || !strings.Contains(string(b), "started: 2026-01-02T03:04:05Z") {
			t.Errorf("lock diagnostics: %q", b)
		}
		l.unlock()
	}
}

// TestLockUnknownContentRefused: content neither version writes is not
// taken over.
func TestLockUnknownContentRefused(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, lockName), []byte("garbage\x00\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := lockFile(dir, nil); err == nil || !strings.Contains(err.Error(), "did not write") {
		t.Fatalf("err = %v", err)
	}
}

// TestLockLegacyStartingDuringGrace: an older hephaestus that created the
// file (still empty) right before this one opened it, and writes its pid
// within the grace time, is seen and refused.
func TestLockLegacyStartingDuringGrace(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, lockName)
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		time.Sleep(legacyGrace / 4)
		f, err := os.OpenFile(p, os.O_WRONLY, 0)
		if err == nil {
			_, err = f.WriteString("pid 4242\n")
			_ = f.Close()
		}
		done <- err
	}()
	_, err := lockFile(dir, nil)
	if werr := <-done; werr != nil {
		t.Fatal(werr)
	}
	if err == nil || !strings.Contains(err.Error(), "pid 4242") {
		t.Fatalf("err = %v", err)
	}
}

// TestLockReleasedFile: the file this version leaves (empty) is locked
// again, as is the record of a holder of this version that died.
func TestLockReleasedFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, lockName)
	l, err := lockFile(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockFile(dir, nil); err == nil || !strings.Contains(err.Error(), "pid "+strconv.Itoa(os.Getpid())) {
		t.Errorf("second lock in the same process: %v", err)
	}
	l.unlock()
	if b, err := os.ReadFile(p); err != nil || len(b) != 0 {
		t.Errorf("after unlock the file is kept and empty: %q %v", b, err)
	}
	l, err = lockFile(dir, nil)
	if err != nil {
		t.Fatalf("relock: %v", err)
	}
	l.release(false) // as if killed: the record stays
	l, err = lockFile(dir, nil)
	if err != nil {
		t.Fatalf("after a holder of this version died: %v", err)
	}
	l.unlock()
}

// legacyLockFile is the lock of hephaestus before the file lock (verbatim
// apart from the names), to run both versions against one clone.
func legacyLockFile(dir string) (func(), error) {
	p := filepath.Join(dir, lockName)
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	_, werr := f.WriteString("pid " + strconv.Itoa(os.Getpid()) + "\n")
	if err := errors.Join(werr, f.Close()); err != nil {
		_ = os.Remove(p)
		return nil, err
	}
	return func() { _ = os.Remove(p) }, nil
}

// TestLockMixedVersions: the older (O_EXCL) and this version exclude each
// other in one clone, in either order.
func TestLockMixedVersions(t *testing.T) {
	t.Run("new first", func(t *testing.T) {
		dir := t.TempDir()
		l, err := lockFile(dir, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := legacyLockFile(dir); !errors.Is(err, fs.ErrExist) {
			t.Fatalf("older version while this one holds: %v", err)
		}
		l.unlock()
		// The released file stays, so the older version keeps refusing
		// rather than running beside a later publish of this version.
		if _, err := legacyLockFile(dir); !errors.Is(err, fs.ErrExist) {
			t.Fatalf("older version after release: %v", err)
		}
	})
	t.Run("old first", func(t *testing.T) {
		dir := t.TempDir()
		release, err := legacyLockFile(dir)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := lockFile(dir, nil); err == nil || !strings.Contains(err.Error(), "older hephaestus") {
			t.Fatalf("this version while an older one runs: %v", err)
		}
		release()
		l, err := lockFile(dir, nil)
		if err != nil {
			t.Fatalf("after the older version finished: %v", err)
		}
		l.unlock()
	})
}

// TestLockVerify: a held lock whose file is removed or replaced (another
// process may then lock the new file) or rewritten no longer verifies.
func TestLockVerify(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, lockName)
	l, err := lockFile(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.verify(); err != nil {
		t.Fatalf("held: %v", err)
	}
	if err := os.WriteFile(p, []byte("pid: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := l.verify(); err == nil || !strings.Contains(err.Error(), "rewritten") {
		t.Errorf("rewritten: %v", err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := l.verify(); err == nil || !strings.Contains(err.Error(), "removed") {
		t.Errorf("removed: %v", err)
	}
	other, err := lockFile(dir, nil) // what the removal allows
	if err != nil {
		t.Fatal(err)
	}
	if err := l.verify(); err == nil || !strings.Contains(err.Error(), "replaced") {
		t.Errorf("replaced: %v", err)
	}
	if err := other.verify(); err != nil {
		t.Errorf("the new holder: %v", err)
	}
	other.unlock()
	l.unlock()
}

// TestLockRefusesUnsafeFileSystem: a file system where a lock may hold only
// on this host is refused before the file is touched.
func TestLockRefusesUnsafeFileSystem(t *testing.T) {
	dir := t.TempDir()
	if err := lockableFS(dir); err != nil {
		t.Fatalf("the test's temporary directory: %v", err)
	}
	for name, want := range map[string]bool{
		"apfs": false, "hfs": false, "ufs": false, "zfs": false, "tmpfs": false, "msdos": false,
		"nfs": true, "smbfs": true, "afpfs": true, "webdav": true, "macfuse": true, "osxfuse": true, "fusefs.sshfs": true, "9p": true,
	} {
		if got := remoteFSName(name); got != want {
			t.Errorf("remoteFSName(%q) = %v, want %v", name, got, want)
		}
	}
}

// TestLockContention: of many processes started at once on one clone,
// exactly one holds the lock; the others are told who.
func TestLockContention(t *testing.T) {
	dir := t.TempDir()
	const n = 8
	results := make(chan string, n)
	for range n {
		c := helperCmd("try", dir)
		out, err := c.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Process.Kill(); _ = c.Wait() })
		go func() {
			line, _ := bufio.NewReader(out).ReadString('\n')
			results <- line
		}()
	}
	locked := 0
	for range n {
		line := <-results
		switch {
		case strings.HasPrefix(line, "locked "):
			locked++
		case strings.Contains(line, "another publish is running"):
		default:
			t.Errorf("helper: %q", line)
		}
	}
	if locked != 1 {
		t.Fatalf("%d processes hold the lock, want 1", locked)
	}
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
