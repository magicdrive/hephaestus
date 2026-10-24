package homebrew

import (
	"os"
	"path/filepath"
	"testing"
)

// A dist directory that links out of the project root must make the formula
// stage fail without creating anything outside the root, not even the
// homebrew/Formula directories (F7/F18).
func TestRunDistLinkOutsideCreatesNothing(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "proj")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{root, outside} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("../outside", filepath.Join(root, "dist")); err != nil {
		t.Skip(err)
	}
	in := input(baseBrew(), fourPlatforms(false)...)
	in.Config.Root = root
	if _, err := Run(in); err == nil {
		t.Fatal("formula written through a dist link that leaves the root")
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("created outside the root: %v", entries)
	}
}

func TestWriteConfined(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "proj")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeConfined(root, "dist/homebrew/Formula/x.rb", []byte("a")); err != nil {
		t.Fatal(err)
	}
	if err := writeConfined(root, "dist/homebrew/Formula/x.rb", []byte("b")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "dist", "homebrew", "Formula", "x.rb")); string(b) != "b" {
		t.Errorf("content = %q", b)
	}
	// A link inside the root that stays inside is fine.
	if err := os.Mkdir(filepath.Join(root, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(root, "alias")); err != nil {
		t.Skip(err)
	}
	if err := writeConfined(root, "alias/y.rb", []byte("y")); err != nil {
		t.Errorf("link inside the root: %v", err)
	}
	// The output itself being a link is refused, and its target untouched.
	victim := filepath.Join(base, "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(root, "dist", "homebrew", "Formula", "z.rb")); err != nil {
		t.Fatal(err)
	}
	if err := writeConfined(root, "dist/homebrew/Formula/z.rb", []byte("z")); err == nil {
		t.Error("wrote over a symbolic link")
	}
	if b, _ := os.ReadFile(victim); string(b) != "keep" {
		t.Errorf("victim = %q", b)
	}
	for _, bad := range []string{"../x.rb", "/abs.rb"} {
		if err := writeConfined(root, bad, []byte("x")); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
