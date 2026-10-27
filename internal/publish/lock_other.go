//go:build !(darwin || dragonfly || freebsd || linux || netbsd || openbsd || windows)

package publish

import (
	"fmt"
	"os"
	"runtime"
)

// tryLock: publish has no file lock on this system and refuses to run
// unlocked.
func tryLock(*os.File) error {
	return fmt.Errorf("file locks are not implemented on %s", runtime.GOOS)
}

func unlockFile(*os.File) error { return nil }
