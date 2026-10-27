package publish

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// lockName is the publish lock file in the git common directory.
const lockName = "hephaestus-publish.lock"

// errLockBusy is returned by tryLock when another open file holds the lock.
var errLockBusy = errors.New("lock held")

// legacyLock is the whole content of the lock file of a hephaestus older
// than the file lock (it created the file with O_EXCL, wrote "pid N" and
// removed the file when it finished).
var legacyLock = regexp.MustCompile(`^pid [0-9]+\n?$`)

// legacyGrace is how long a lock file that existed but was empty is
// watched before it is taken: an older hephaestus creates the file (empty)
// and writes its pid right after.
var legacyGrace = 200 * time.Millisecond

// fileLock is a held publish lock.
type fileLock struct {
	f    *os.File
	dir  string
	info string // what this process wrote into the file
}

// lock prevents two publish processes in one clone (make -j, a second
// terminal, a second worktree). Release it with unlock; verify reports
// whether it is still the lock of the file at its path.
//
// Exclusion is an operating system advisory lock on the lock file (flock on
// Unix, LockFileEx on Windows), never the file's existence or the PID in
// it. The kernel drops the lock when the owning process exits for any
// reason, SIGKILL and crashes included, so a lock left by a dead publish is
// reclaimed by the next one without any judgement about PIDs, hosts or
// clocks, and a live owner's lock cannot be taken: there is no forced
// takeover. The lock is not inherited by the git and gh commands publish
// runs (Go opens files close-on-exec and handles non-inheritable).
//
// The file's contents (pid, host, start time, boot id) are diagnostics,
// shown when the lock is busy, and tell this version from an older one.
// The file is never removed: removing a lock file lets a waiter lock the
// unlinked inode while a third process locks a new file of the same name.
//
// Older hephaestus versions locked by creating the same file with O_EXCL,
// without a file lock. Since this version never removes the file, they
// always find it and refuse. The other way round, their file ("pid N")
// holds no file lock, so it cannot tell a running older publish from one
// that was killed: this version refuses (it never judges the pid) until
// someone who has checked empties the file. An empty file that existed is
// watched for legacyGrace first, the time an older version takes between
// creating the file and writing its pid. Content that is neither format is
// refused too.
//
// When the file system cannot lock (some NFS mounts, FUSE), or is a network
// or user-space file system where a lock that succeeds may be local to
// this host (NFS mounted with nolock or local_lock), publish refuses instead
// of running with a lock that may not exclude anything. Separate clones and
// machines are never excluded by the lock: the tag on GitHub then decides
// (a push is never forced and the tag ruleset forbids updating it).
func lock(ctx context.Context, d *Deps) (*fileLock, error) {
	dir, err := d.Git.CommonDir(ctx)
	if err != nil {
		return nil, err
	}
	return lockFile(dir, d.Now)
}

// lockFile locks lockName in dir.
func lockFile(dir string, now func() time.Time) (*fileLock, error) {
	p := filepath.Join(dir, lockName)
	if err := lockableFS(dir); err != nil {
		return nil, fmt.Errorf("refusing to lock %s: %w; run publish from a clone on a local disk", p, err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("open publish lock: %w", err)
	}
	defer root.Close()
	created := true
	f, err := root.OpenFile(lockName, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		created = false
		f, err = root.OpenFile(lockName, os.O_RDWR|os.O_CREATE, 0o600)
	}
	if err != nil {
		return nil, fmt.Errorf("open publish lock: %w", err)
	}
	if err := tryLock(f); err != nil {
		holder := lockHolder(root)
		_ = f.Close()
		if errors.Is(err, errLockBusy) {
			return nil, fmt.Errorf("another publish is running in this clone (%s; lock %s). "+
				"The lock is released when that process exits, also when it is killed or crashes: "+
				"wait for it or stop it, then run publish again; do not remove the file", holder, p)
		}
		return nil, fmt.Errorf("cannot lock %s (%w): this file system may not support file locks; "+
			"run publish from a clone on a local disk", p, err)
	}
	l := &fileLock{f: f, dir: dir}
	refuse := func(err error) (*fileLock, error) {
		l.release(false)
		return nil, err
	}
	if err := l.samePath(root); err != nil {
		return refuse(fmt.Errorf("publish lock %s: %w; run publish again", p, err))
	}
	content, err := readLock(f)
	if err != nil {
		return refuse(fmt.Errorf("read publish lock %s: %w", p, err))
	}
	if content == "" && !created {
		time.Sleep(legacyGrace)
		if content, err = readLock(f); err != nil {
			return refuse(fmt.Errorf("read publish lock %s: %w", p, err))
		}
	}
	switch {
	case content == "" || strings.HasPrefix(content, "pid: "):
		// Released, or held by a process of this version that died.
	case legacyLock.MatchString(content):
		return refuse(fmt.Errorf("%s was written by an older hephaestus (%q), which does not take the file lock: "+
			"it may still be publishing from this clone. Make sure no older `make publish` runs in this clone "+
			"(or any of its worktrees), then empty the file (`: > %s`; do not remove it) and run publish again",
			p, strings.TrimSpace(content), p))
	default:
		return refuse(fmt.Errorf("%s has content this version did not write (%q): it may belong to another tool or version. "+
			"Make sure no other publish runs in this clone, then empty the file (`: > %s`; do not remove it) and run publish again",
			p, truncateForMessage(content), p))
	}
	if now == nil {
		now = time.Now
	}
	l.info = lockInfo(now())
	if err := f.Truncate(0); err != nil {
		return refuse(fmt.Errorf("write publish lock %s: %w", p, err))
	}
	if _, err := f.WriteAt([]byte(l.info), 0); err != nil {
		return refuse(fmt.Errorf("write publish lock %s: %w", p, err))
	}
	return l, nil
}

// unlock releases the lock and leaves the file empty (released).
func (l *fileLock) unlock() { l.release(true) }

func (l *fileLock) release(empty bool) {
	if empty {
		_ = l.f.Truncate(0)
	}
	_ = unlockFile(l.f)
	_ = l.f.Close()
}

// verify reports whether the lock still guards its path: the file at the
// path is the locked file and still holds this process's record. It fails
// when the file was removed or replaced (as an older version's message
// advised), so that another process may hold a lock on the new file.
func (l *fileLock) verify() error {
	p := filepath.Join(l.dir, lockName)
	root, err := os.OpenRoot(l.dir)
	if err != nil {
		return fmt.Errorf("publish lock %s: %w", p, err)
	}
	defer root.Close()
	if err := l.samePath(root); err != nil {
		return fmt.Errorf("publish lock %s: %w", p, err)
	}
	content, err := readLock(l.f)
	if err != nil {
		return fmt.Errorf("publish lock %s: %w", p, err)
	}
	if content != l.info {
		return fmt.Errorf("publish lock %s was rewritten by someone else (%q)", p, truncateForMessage(content))
	}
	return nil
}

// samePath fails unless lockName in root is the locked file.
func (l *fileLock) samePath(root *os.Root) error {
	held, err := l.f.Stat()
	if err != nil {
		return err
	}
	cur, err := root.Stat(lockName)
	if err != nil {
		return fmt.Errorf("the lock file was removed while held (%w)", err)
	}
	if !os.SameFile(held, cur) {
		return errors.New("the lock file was replaced while held")
	}
	return nil
}

// readLock reads the whole lock file (at most 4 KiB).
func readLock(f *os.File) (string, error) {
	buf := make([]byte, 4096)
	n, err := f.ReadAt(buf, 0)
	if err != nil && n == 0 && !errors.Is(err, io.EOF) {
		return "", err
	}
	return string(buf[:n]), nil
}

func truncateForMessage(s string) string {
	if len(s) > 64 {
		return s[:64] + "..."
	}
	return s
}

// lockInfo is the diagnostic content of a held lock file.
func lockInfo(start time.Time) string {
	host, _ := os.Hostname()
	var b strings.Builder
	fmt.Fprintf(&b, "pid: %d\nhost: %s\nstarted: %s\n", os.Getpid(), host, start.UTC().Format(time.RFC3339))
	if id := bootID(); id != "" {
		fmt.Fprintf(&b, "boot_id: %s\n", id)
	}
	return b.String()
}

// bootID identifies the current boot on Linux; "" elsewhere.
func bootID() string {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// lockHolder describes the holder of a busy lock from the file's
// diagnostics. It is informational: the lock, not this text, decides.
func lockHolder(root *os.Root) string {
	b, err := root.ReadFile(lockName)
	if err != nil {
		return "holder unknown"
	}
	fields := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok {
			fields[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	pid, err := strconv.Atoi(fields["pid"])
	if err != nil {
		return "holder unknown"
	}
	s := "pid " + strconv.Itoa(pid)
	if h := fields["host"]; h != "" {
		s += " on " + h
	}
	if t := fields["started"]; t != "" {
		s += " since " + t
	}
	return s
}
