package homebrew

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/magicdrive/hephaestus/internal/config"
	"github.com/magicdrive/hephaestus/internal/tmpl"
)

// DefaultCommitMessage is GoReleaser's default commit_msg_template.
const DefaultCommitMessage = "Brew formula update for {{ .ProjectName }} version {{ .Tag }}"

// DefaultPRBranch is the head branch used in pull request mode when
// repository.branch is empty (GoReleaser v2.17+ uses the same template).
const DefaultPRBranch = "{{ .ProjectName }}-{{ .Version }}"

// Publish is the resolved tap publication plan for one formula. The Tap
// Publisher executes it; nothing here talks to GitHub.
type Publish struct {
	// Skip is true when skip_upload says not to publish; SkipReason says why.
	Skip       bool
	SkipReason string

	// Owner and Name identify the tap repository the formula is pushed to.
	Owner, Name string
	// Path is the formula's path inside the tap: [directory/]<name>.rb.
	Path string

	// PullRequest selects pull request mode (the default, see ResolvePublish).
	PullRequest bool
	Draft       bool
	// Branch is the branch the commit is written to. In pull request mode it
	// is the head branch; in direct mode it may be empty, meaning the
	// repository's default branch.
	Branch string
	// BaseOwner, BaseName and BaseBranch are the pull request target. Empty
	// owner/name mean the tap repository itself; an empty branch means its
	// default branch.
	BaseOwner, BaseName, BaseBranch string

	// AuthorName and AuthorEmail are the commit author. Both empty means the
	// identity of the token (GitHub's default for API commits).
	AuthorName, AuthorEmail string
	Message                 string

	// TokenTemplate is repository.token, not yet rendered, so the secret is
	// read only when needed (see Token). Empty means the default GitHub
	// token.
	TokenTemplate string
}

// Token renders TokenTemplate. An empty result means "use the default token".
func (p Publish) Token(vars tmpl.Vars) (string, error) {
	t, err := tmpl.Render(p.TokenTemplate, vars)
	if err != nil {
		return "", fmt.Errorf("repository.token: %w", err)
	}
	return strings.TrimSpace(t), nil
}

var (
	githubNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	emailRe      = regexp.MustCompile(`^[^\s@<>]+@[^\s@<>]+$`)
)

// ResolvePublish resolves the publication settings of one brews entry.
//
// Differences from GoReleaser, by design:
//   - Pull request mode is the default: unless repository.pull_request.enabled
//     is explicitly false, the formula is committed to a branch and a pull
//     request is opened. GoReleaser pushes directly to the default branch
//     unless pull_request.enabled is true.
//   - There is no default commit author. GoReleaser commits as
//     "goreleaserbot <bot@goreleaser.com>"; Hephaestus leaves the author to
//     the token's identity rather than impersonate another tool's bot.
//
// As in GoReleaser, the commit message defaults to DefaultCommitMessage, the
// pull request head branch to DefaultPRBranch, skip_upload "true" always
// skips and "auto" skips prereleases.
func ResolvePublish(b *config.Brew, vars tmpl.Vars) (Publish, error) {
	var p Publish
	render := func(field, s string) (string, error) {
		r, err := tmpl.Render(s, vars)
		if err != nil {
			return "", fmt.Errorf("%s: %w", field, err)
		}
		return strings.TrimSpace(r), nil
	}
	var errs []error
	check := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}

	switch strings.TrimSpace(b.SkipUpload) {
	case "true":
		p.Skip, p.SkipReason = true, "skip_upload is true"
	case "auto":
		if vars.Prerelease != "" {
			p.Skip, p.SkipReason = true, fmt.Sprintf("skip_upload is auto and %s is a prerelease", vars.Version)
		}
	case "", "false":
	default:
		return p, fmt.Errorf("skip_upload %q must be true, false or auto", b.SkipUpload)
	}

	var err error
	p.Owner, err = render("repository.owner", b.Repository.Owner)
	check(err)
	p.Name, err = render("repository.name", b.Repository.Name)
	check(err)
	if !p.Skip {
		for _, f := range []struct{ field, v string }{{"repository.owner", p.Owner}, {"repository.name", p.Name}} {
			if f.v == "" {
				errs = append(errs, fmt.Errorf("%s is required to publish the formula", f.field))
			} else if !githubNameRe.MatchString(f.v) {
				errs = append(errs, fmt.Errorf("%s %q is not a valid GitHub name", f.field, f.v))
			}
		}
	}

	dir, err := render("directory", b.Directory)
	check(err)
	p.Path = b.Name + ".rb"
	if dir != "" {
		p.Path = strings.Trim(dir, "/") + "/" + p.Path
	}

	pr := b.Repository.PullRequest
	p.PullRequest = pr == nil || pr.Enabled == nil || *pr.Enabled
	if pr != nil {
		p.Draft = pr.Draft
		if pr.Base != nil {
			p.BaseOwner, err = render("repository.pull_request.base.owner", pr.Base.Owner)
			check(err)
			p.BaseName, err = render("repository.pull_request.base.name", pr.Base.Name)
			check(err)
			p.BaseBranch, err = render("repository.pull_request.base.branch", pr.Base.Branch)
			check(err)
		}
	}
	if (p.BaseOwner == "") != (p.BaseName == "") {
		errs = append(errs, errors.New("repository.pull_request.base needs both owner and name, or neither"))
	}
	for _, f := range []struct{ field, v string }{{"repository.pull_request.base.owner", p.BaseOwner}, {"repository.pull_request.base.name", p.BaseName}} {
		if f.v != "" && !githubNameRe.MatchString(f.v) {
			errs = append(errs, fmt.Errorf("%s %q is not a valid GitHub name", f.field, f.v))
		}
	}

	p.Branch, err = render("repository.branch", b.Repository.Branch)
	check(err)
	if p.Branch == "" && p.PullRequest {
		p.Branch, err = render("repository.branch", DefaultPRBranch)
		check(err)
	}
	for _, f := range []struct{ field, v string }{{"repository.branch", p.Branch}, {"repository.pull_request.base.branch", p.BaseBranch}} {
		if f.v != "" && !ValidBranchName(f.v) {
			errs = append(errs, fmt.Errorf("%s %q is not a valid branch name", f.field, f.v))
		}
	}
	if p.PullRequest && p.Branch != "" && p.Branch == p.BaseBranch && p.BaseOwner == "" {
		errs = append(errs, fmt.Errorf("repository.branch %q is also the pull request base branch", p.Branch))
	}

	p.AuthorName, err = render("commit_author.name", b.CommitAuthor.Name)
	check(err)
	p.AuthorEmail, err = render("commit_author.email", b.CommitAuthor.Email)
	check(err)
	if (p.AuthorName == "") != (p.AuthorEmail == "") {
		errs = append(errs, errors.New("commit_author needs both name and email, or neither"))
	}
	if strings.ContainsAny(p.AuthorName, "<>\r\n") {
		errs = append(errs, fmt.Errorf("commit_author.name %q contains invalid characters", p.AuthorName))
	}
	if p.AuthorEmail != "" && !emailRe.MatchString(p.AuthorEmail) {
		errs = append(errs, fmt.Errorf("commit_author.email %q is not an email address", p.AuthorEmail))
	}

	msg := b.CommitMsg
	if msg == "" {
		msg = DefaultCommitMessage
	}
	p.Message, err = render("commit_msg_template", msg)
	check(err)
	if err == nil && p.Message == "" {
		errs = append(errs, errors.New("commit_msg_template renders to an empty message"))
	}

	p.TokenTemplate = b.Repository.Token
	switch b.Repository.TokenType {
	case "", "github":
	default:
		errs = append(errs, fmt.Errorf("repository.token_type %q is not supported; only github", b.Repository.TokenType))
	}
	if err := errors.Join(errs...); err != nil {
		return Publish{}, fmt.Errorf("formula %s: %w", b.Name, err)
	}
	return p, nil
}

// ValidBranchName applies git check-ref-format rules to a branch name.
func ValidBranchName(s string) bool {
	if s == "" || s == "@" || len(s) > 255 || strings.HasPrefix(s, "-") || strings.HasPrefix(s, "/") ||
		strings.HasSuffix(s, "/") || strings.HasSuffix(s, ".") || strings.HasSuffix(s, ".lock") ||
		strings.Contains(s, "..") || strings.Contains(s, "//") || strings.Contains(s, "@{") {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(" ~^:?*[\\", r) {
			return false
		}
	}
	for _, seg := range strings.Split(s, "/") {
		if strings.HasPrefix(seg, ".") || strings.HasSuffix(seg, ".lock") {
			return false
		}
	}
	return true
}
