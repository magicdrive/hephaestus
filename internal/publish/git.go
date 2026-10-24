package publish

import (
	"context"
	"fmt"
	"strings"

	"github.com/magicdrive/hephaestus/internal/github"
)

// Git is the git access publish needs. Every method is read-only except
// Fetch (updates refs/remotes/origin/main), CreateTag and PushTag.
type Git interface {
	TopLevel(ctx context.Context) (string, error)
	CommonDir(ctx context.Context) (string, error)
	// CurrentBranch returns "" for a detached HEAD.
	CurrentBranch(ctx context.Context) (string, error)
	// Status returns `git status --porcelain` including untracked files.
	Status(ctx context.Context) (string, error)
	// RemoteURL returns the fetch URL, or with push the URL git pushes to
	// (pushurl / pushInsteadOf applied).
	RemoteURL(ctx context.Context, remote string, push bool) (string, error)
	// FetchMain updates refs/remotes/origin/main without fetching tags.
	FetchMain(ctx context.Context) error
	// Commit resolves ref to a commit SHA; "" when it does not exist.
	Commit(ctx context.Context, ref string) (string, error)
	// RemoteTag returns the commit a tag on origin points to (peeled), and
	// whether it exists.
	RemoteTag(ctx context.Context, tag string) (string, bool, error)
	// CreateTag creates an annotated tag; it fails if the tag exists.
	CreateTag(ctx context.Context, tag, commit, message string) error
	// PushTag pushes one tag to origin, never forced.
	PushTag(ctx context.Context, tag string) error
}

// GitCLI implements Git with the git command.
type GitCLI struct{ Run Runner }

func (g GitCLI) out(ctx context.Context, args ...string) (string, error) {
	b, err := g.Run(ctx, "git", args...)
	return strings.TrimSpace(string(b)), err
}

func (g GitCLI) TopLevel(ctx context.Context) (string, error) {
	return g.out(ctx, "rev-parse", "--show-toplevel")
}

func (g GitCLI) CommonDir(ctx context.Context) (string, error) {
	return g.out(ctx, "rev-parse", "--path-format=absolute", "--git-common-dir")
}

func (g GitCLI) CurrentBranch(ctx context.Context) (string, error) {
	b, err := g.out(ctx, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil && exitCode(err) == 1 {
		return "", nil // detached
	}
	return b, err
}

func (g GitCLI) Status(ctx context.Context) (string, error) {
	return g.out(ctx, "status", "--porcelain=v1", "--untracked-files=all")
}

func (g GitCLI) RemoteURL(ctx context.Context, remote string, push bool) (string, error) {
	if push {
		return g.out(ctx, "remote", "get-url", "--push", remote)
	}
	return g.out(ctx, "remote", "get-url", remote)
}

func (g GitCLI) FetchMain(ctx context.Context) error {
	_, err := g.out(ctx, "fetch", "--quiet", "--no-tags", "origin", "+refs/heads/main:refs/remotes/origin/main")
	return err
}

func (g GitCLI) Commit(ctx context.Context, ref string) (string, error) {
	c, err := g.out(ctx, "rev-parse", "--verify", "--quiet", "--end-of-options", ref+"^{commit}")
	if err != nil && exitCode(err) == 1 {
		return "", nil
	}
	return c, err
}

func (g GitCLI) RemoteTag(ctx context.Context, tag string) (string, bool, error) {
	out, err := g.out(ctx, "ls-remote", "--tags", "origin", "refs/tags/"+tag, "refs/tags/"+tag+"^{}")
	if err != nil {
		return "", false, err
	}
	return parseLsRemoteTag(out, tag)
}

// parseLsRemoteTag reads `git ls-remote` output for one tag: the peeled
// line (annotated tag) wins over the tag object's own line.
func parseLsRemoteTag(out, tag string) (string, bool, error) {
	var direct, peeled string
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) != 2 {
			continue
		}
		switch f[1] {
		case "refs/tags/" + tag:
			direct = f[0]
		case "refs/tags/" + tag + "^{}":
			peeled = f[0]
		}
	}
	switch {
	case peeled != "":
		return peeled, true, nil
	case direct != "":
		return direct, true, nil
	}
	return "", false, nil
}

func (g GitCLI) CreateTag(ctx context.Context, tag, commit, message string) error {
	_, err := g.out(ctx, "tag", "--annotate", "--message", message, "--end-of-options", tag, commit)
	return err
}

func (g GitCLI) PushTag(ctx context.Context, tag string) error {
	ref := "refs/tags/" + tag
	// --no-follow-tags overrides push.followTags: only this one tag goes.
	_, err := g.out(ctx, "-c", "push.followTags=false", "push", "--no-follow-tags", "--porcelain", "--no-verify", "origin", ref+":"+ref)
	return err
}

// ParseGitHubRemote returns the owner/name of a github.com remote URL in
// any of the forms git accepts: https://github.com/o/r(.git),
// ssh://git@github.com[:22]/o/r(.git), git@github.com:o/r(.git).
func ParseGitHubRemote(raw string) (Repo, error) {
	host, owner, name, err := github.ParseRemoteURL(raw)
	if err != nil {
		return Repo{}, err
	}
	if host != "github.com" {
		return Repo{}, fmt.Errorf("remote %q is not on github.com", raw)
	}
	return Repo{Owner: owner, Name: name}, nil
}
