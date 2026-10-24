package manifest

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func openRoot(t *testing.T, dir string) *os.Root {
	t.Helper()
	r, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func TestWriteFileInRoundTrip(t *testing.T) {
	dir := t.TempDir()
	root := openRoot(t, dir)
	if err := sample().WriteFileIn(root, "dist/sub/m.json"); err != nil {
		t.Fatal(err)
	}
	// Overwrite in place.
	if err := sample().WriteFileIn(root, "dist/sub/m.json"); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFileIn(root, "dist/sub/m.json"); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "dist", "sub"))
	if len(entries) != 1 {
		t.Errorf("temporary files left: %v", entries)
	}
	st, err := os.Stat(filepath.Join(dir, "dist", "sub", "m.json"))
	if err != nil || (runtime.GOOS != "windows" && st.Mode().Perm() != 0o644) {
		t.Errorf("mode = %v, %v", st, err)
	}
}

func TestWriteFileAtomicInRejectsBadPaths(t *testing.T) {
	root := openRoot(t, t.TempDir())
	for _, rel := range []string{"../x", "/abs", "a//b", ""} {
		if err := WriteFileAtomicIn(root, rel, []byte("x"), 0o644); err == nil {
			t.Errorf("%q accepted", rel)
		}
	}
}

// A dist directory that is a symbolic link out of the root must not let any
// file or directory be created outside it (F7).
func TestWriteFileAtomicInConfinedToRoot(t *testing.T) {
	base := t.TempDir()
	proj := filepath.Join(base, "proj")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{proj, outside} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("../outside", filepath.Join(proj, "dist")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(proj, "abs")); err != nil {
		t.Fatal(err)
	}
	root := openRoot(t, proj)
	for _, rel := range []string{"dist/m.json", "dist/homebrew/Formula/x.rb", "abs/m.json", "abs/deep/m.json"} {
		if err := WriteFileAtomicIn(root, rel, []byte("x"), 0o644); err == nil {
			t.Errorf("%s: write through an escaping link succeeded", rel)
		}
		if err := sample().WriteFileIn(root, rel); err == nil {
			t.Errorf("%s: manifest write through an escaping link succeeded", rel)
		}
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("created outside the root: %v", entries)
	}
	if _, err := ReadFileIn(root, "dist/m.json"); err == nil {
		t.Error("read through an escaping link succeeded")
	}
}

// An existing output that is a symbolic link is replaced, never written
// through.
func TestWriteFileAtomicInReplacesLinkNotTarget(t *testing.T) {
	base := t.TempDir()
	proj := filepath.Join(base, "proj")
	if err := os.Mkdir(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(base, "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(proj, "out")); err != nil {
		t.Fatal(err)
	}
	root := openRoot(t, proj)
	if err := WriteFileAtomicIn(root, "out", []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(victim); string(b) != "keep" {
		t.Errorf("link target was overwritten: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(proj, "out")); string(b) != "new" {
		t.Errorf("out = %q", b)
	}
}
