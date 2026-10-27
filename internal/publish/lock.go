package publish

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// lockName is the publish lock file in the git common directory.
const lockName = "hephaestus-publish.lock"

// errLockBusy is returned by tryLock when another open file holds the lock.
var errLockBusy = errors.New("lock held")

// lock prevents two publish processes in one clone (make -j, a second
// terminal, a second worktree). It returns the function that releases the
// lock.
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
// The file's contents (pid, host, start time, boot id) are diagnostics
// only, shown when the lock is busy. The file is never removed: removing a
// lock file lets a waiter lock the unlinked inode while a third process
// locks a new file of the same name. A leftover file, including one left by
// an older hephaestus that locked by O_EXCL, is simply locked again.
//
// When the file system cannot lock (some NFS mounts, FUSE), publish
// refuses instead of running unlocked. Where locks are local to one host
// (an NFS mount with nolock), two hosts sharing a clone are not excluded:
// as for separate clones, the tag on GitHub then decides (a push is never
// forced and the tag ruleset forbids updating it).
func lock(ctx context.Context, d *Deps) (func(), error) {
	dir, err := d.Git.CommonDir(ctx)
	if err != nil {
		return nil, err
	}
	return lockFile(dir, d.Now)
}

// lockFile locks lockName in dir.
func lockFile(dir string, now func() time.Time) (func(), error) {
	p := filepath.Join(dir, lockName)
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("open publish lock: %w", err)
	}
	defer root.Close()
	f, err := root.OpenFile(lockName, os.O_RDWR|os.O_CREATE, 0o600)
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
	if now == nil {
		now = time.Now
	}
	// Diagnostics only: a failure to record them does not matter.
	if f.Truncate(0) == nil {
		_, _ = f.WriteAt([]byte(lockInfo(now())), 0)
	}
	return func() {
		_ = f.Truncate(0)
		_ = unlockFile(f)
		_ = f.Close()
	}, nil
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
