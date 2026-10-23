package manifest

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"

	"github.com/magicdrive/hephaestus/internal/pathutil"
)

// The functions in this file confine every file system access to an
// os.Root: a symbolic link (or a directory swapped for one while Hephaestus
// runs) can never make them create, replace or read anything outside the
// root. Paths are slash-separated and relative to the root.

// ReadFileIn reads a manifest at rel inside root.
func ReadFileIn(root *os.Root, rel string) (*Manifest, error) {
	if err := pathutil.ValidateRelative(rel); err != nil {
		return nil, fmt.Errorf("manifest path %q: %w", rel, err)
	}
	f, err := root.Open(rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	m, err := Read(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", rel, err)
	}
	return m, nil
}

// WriteFileIn writes the canonical serialization to rel inside root,
// atomically (see WriteFileAtomicIn).
func (m *Manifest) WriteFileIn(root *os.Root, rel string) error {
	data, err := m.Marshal()
	if err != nil {
		return err
	}
	return WriteFileAtomicIn(root, rel, data, 0o644)
}

// WriteFileAtomicIn writes data to rel inside root via a temporary file in
// the same directory and a rename, so a crash never leaves a torn file.
// Missing parent directories are created inside root. A parent that is (or
// passes through) a symbolic link resolving outside root is an error, and
// in that case nothing is created outside root.
func WriteFileAtomicIn(root *os.Root, rel string, data []byte, perm os.FileMode) error {
	if err := pathutil.ValidateRelative(rel); err != nil {
		return fmt.Errorf("output path %q: %w", rel, err)
	}
	if dir := path.Dir(rel); dir != "." {
		if err := root.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp, name, err := CreateTempIn(root, rel)
	if err != nil {
		return err
	}
	defer root.Remove(name) //nolint:errcheck // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close() // already failing; the original error takes precedence
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close() // already failing; the original error takes precedence
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close() // already failing; the original error takes precedence
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return root.Rename(name, rel)
}

// CreateTempIn creates a new, exclusive temporary file next to rel inside
// root (".tmp-<base>-<random>") and returns it with its root-relative name.
// The parent directory must already exist. The caller renames the file into
// place with root.Rename, or removes it with root.Remove.
func CreateTempIn(root *os.Root, rel string) (*os.File, string, error) {
	dir := path.Dir(rel)
	for range 16 {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			return nil, "", err
		}
		name := ".tmp-" + path.Base(rel) + "-" + hex.EncodeToString(b[:])
		if dir != "." {
			name = dir + "/" + name
		}
		f, err := root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return nil, "", err
		}
		return f, name, nil
	}
	return nil, "", fmt.Errorf("create temporary file next to %s: too many collisions", rel)
}

// OpenConfined opens the project-relative file rel for reading through an
// os.Root at rootDir, so neither the path nor a symbolic link along it can
// reach outside the project. The returned file stays valid after the root
// handle is closed.
func OpenConfined(rootDir, rel string) (*os.File, error) {
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return root.Open(filepath.FromSlash(rel))
}
