package preflight

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Git is the read-only git access the preflight needs.
type Git interface {
	// TopLevel returns the work tree root; an error outside a repository.
	TopLevel(ctx context.Context) (string, error)
	// Head returns the commit HEAD points to.
	Head(ctx context.Context) (string, error)
	// Branch returns the current branch, or "" for a detached HEAD.
	Branch(ctx context.Context) (string, error)
	// Status returns `git status --porcelain` including untracked files.
	Status(ctx context.Context) (string, error)
	// RemoteURL returns a remote's fetch URL, or with push its push URL.
	RemoteURL(ctx context.Context, remote string, push bool) (string, error)
	// LocalTag returns the commit refs/tags/<tag> points to, or "".
	LocalTag(ctx context.Context, tag string) (string, error)
	// RemoteDefaultBranch returns the branch refs/remotes/<remote>/HEAD
	// points to, or "".
	RemoteDefaultBranch(ctx context.Context, remote string) (string, error)
	// LsRemote lists the remote's branches and tags (peeled tags as
	// "refs/tags/x^{}") from the network.
	LsRemote(ctx context.Context, remote string) (map[string]string, error)
}

// GitCLI is Git through the git command, restricted to read-only
// subcommands. Optional locks are disabled so that even `git status` does
// not rewrite the index, and git never prompts for credentials.
type GitCLI struct {
	Dir string
	// Exec runs git with args in Dir (tests replace it); nil uses os/exec.
	Exec func(ctx context.Context, dir string, args ...string) ([]byte, error)
}

// readOnly are the git subcommands the preflight may run, with the first
// arguments they must have. Anything else is refused before git starts.
var readOnly = map[string]func(args []string) bool{
	"rev-parse":    func([]string) bool { return true },
	"symbolic-ref": func(a []string) bool { return len(a) > 0 && a[0] == "--quiet" },
	"status":       func(a []string) bool { return len(a) > 0 && strings.HasPrefix(a[0], "--porcelain") },
	"remote":       func(a []string) bool { return len(a) > 0 && a[0] == "get-url" },
	"ls-remote":    func([]string) bool { return true },
}

// ErrNotReadOnly is returned for a git command outside the read-only set.
var ErrNotReadOnly = errors.New("git command is not on the read-only list")

func (g GitCLI) run(ctx context.Context, args ...string) (string, error) {
	if len(args) == 0 {
		return "", ErrNotReadOnly
	}
	if ok := readOnly[args[0]]; ok == nil || !ok(args[1:]) {
		return "", fmt.Errorf("%w: git %s", ErrNotReadOnly, strings.Join(args, " "))
	}
	full := append([]string{"--no-optional-locks"}, args...)
	exe := g.Exec
	if exe == nil {
		exe = execGit
	}
	out, err := exe(ctx, g.Dir, full...)
	return strings.TrimSpace(string(out)), err
}

// gitError is a failed git command with its exit code.
type gitError struct {
	args   []string
	code   int
	stderr string
}

func (e *gitError) Error() string {
	msg := strings.TrimSpace(e.stderr)
	if msg == "" {
		msg = fmt.Sprintf("exit status %d", e.code)
	}
	return "git " + strings.Join(e.args, " ") + ": " + msg
}

func exitCode(err error) int {
	var ge *gitError
	if errors.As(err, &ge) {
		return ge.code
	}
	return -1
}

func execGit(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0", "GCM_INTERACTIVE=never")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		ge := &gitError{args: args, code: -1, stderr: errb.String()}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			ge.code = ee.ExitCode()
		} else if ge.stderr == "" {
			ge.stderr = err.Error()
		}
		return out.Bytes(), ge
	}
	return out.Bytes(), nil
}

func (g GitCLI) TopLevel(ctx context.Context) (string, error) {
	return g.run(ctx, "rev-parse", "--show-toplevel")
}

func (g GitCLI) Head(ctx context.Context) (string, error) {
	return g.run(ctx, "rev-parse", "--verify", "--quiet", "HEAD^{commit}")
}

func (g GitCLI) Branch(ctx context.Context) (string, error) {
	b, err := g.run(ctx, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil && exitCode(err) == 1 {
		return "", nil
	}
	return b, err
}

func (g GitCLI) Status(ctx context.Context) (string, error) {
	return g.run(ctx, "status", "--porcelain=v1", "--untracked-files=all")
}

func (g GitCLI) RemoteURL(ctx context.Context, remote string, push bool) (string, error) {
	if push {
		return g.run(ctx, "remote", "get-url", "--push", "--", remote)
	}
	return g.run(ctx, "remote", "get-url", "--", remote)
}

func (g GitCLI) LocalTag(ctx context.Context, tag string) (string, error) {
	c, err := g.run(ctx, "rev-parse", "--verify", "--quiet", "--end-of-options", "refs/tags/"+tag+"^{commit}")
	if err != nil && exitCode(err) == 1 {
		return "", nil
	}
	return c, err
}

func (g GitCLI) RemoteDefaultBranch(ctx context.Context, remote string) (string, error) {
	b, err := g.run(ctx, "symbolic-ref", "--quiet", "--short", "refs/remotes/"+remote+"/HEAD")
	if err != nil && exitCode(err) == 1 {
		return "", nil
	}
	return strings.TrimPrefix(b, remote+"/"), err
}

func (g GitCLI) LsRemote(ctx context.Context, remote string) (map[string]string, error) {
	out, err := g.run(ctx, "ls-remote", "--heads", "--tags", "--", remote)
	if err != nil {
		return nil, err
	}
	refs := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		if sha, ref, ok := strings.Cut(strings.TrimSpace(l), "\t"); ok {
			refs[ref] = sha
		}
	}
	return refs, nil
}
