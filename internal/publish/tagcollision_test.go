package publish

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestTagCollisionRealGit is the last defence when the lock excludes
// nothing (two clones, two hosts, a lock lost on a network file system):
// with real git, the second creation of a tag in one clone fails, and a
// second clone's push of the same tag at another commit is rejected
// without moving origin's tag.
func TestTagCollisionRealGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for k, v := range map[string]string{"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@example.com", "GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@example.com"} {
		t.Setenv(k, v)
	}
	ctx := context.Background()
	run := func(dir string, args ...string) string {
		t.Helper()
		out, err := ExecRunner(ctx, "git", append([]string{"-C", dir}, args...)...)
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	root := t.TempDir()
	work := filepath.Join(root, "work")
	run(root, "init", "--quiet", "-b", "main", work)
	run(work, "commit", "--quiet", "--allow-empty", "-m", "first")
	bare := filepath.Join(root, "origin.git")
	run(root, "clone", "--quiet", "--bare", work, bare)
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	run(root, "clone", "--quiet", bare, a)
	run(root, "clone", "--quiet", bare, b)
	run(b, "commit", "--quiet", "--allow-empty", "-m", "other")
	ca, cb := run(a, "rev-parse", "HEAD"), run(b, "rev-parse", "HEAD")
	inDir := func(dir string) GitCLI {
		return GitCLI{Run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return ExecRunner(ctx, name, append([]string{"-C", dir}, args...)...)
		}}
	}
	ga, gb := inDir(a), inDir(b)
	const tag = "v1.0.0"
	if err := ga.CreateTag(ctx, tag, ca, "Release "+tag); err != nil {
		t.Fatal(err)
	}
	if err := ga.CreateTag(ctx, tag, ca, "Release "+tag); err == nil {
		t.Fatal("a second creation of the tag in one clone succeeded")
	}
	if err := ga.PushTag(ctx, tag); err != nil {
		t.Fatal(err)
	}
	if err := gb.CreateTag(ctx, tag, cb, "Release "+tag); err != nil {
		t.Fatal(err)
	}
	if err := gb.PushTag(ctx, tag); err == nil {
		t.Fatal("the push of the same tag at another commit was accepted")
	}
	if c, ok, err := gb.RemoteTag(ctx, tag); err != nil || !ok || c != ca {
		t.Fatalf("origin's tag: %q %v %v, want %s", c, ok, err, ca)
	}
}
