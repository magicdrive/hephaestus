package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/magicdrive/hephaestus/internal/tap"
)

// Rollback authorization (-rollback, -approvals): under -strict, a
// downgrade passes (as a warning) only when it is exactly an authorized
// one: the same formula path, from the same version, to the same version.
// Every other finding stays an error, a removal included; an
// authorization that matches no downgrade of the change is an error too,
// so an approval always describes the change it lets through.
//
// -rollback PATH=FROM->TO authorizes one by hand (a reviewer re-checking
// locally). -approvals reads the comments of a pull request (the JSON of
// GET /repos/{repo}/issues/{n}/comments, objects or arrays, concatenated)
// and -permissions the answers of GET
// /repos/{repo}/collaborators/{login}/permission for their authors. A
// comment authorizes a rollback only when all of these hold:
//
//   - its body is exactly
//
//     /formula-rollback
//     head: <the 40-digit pull request head SHA, -approval-head>
//     formula: <path> <from> -> <to>     (one or more, each a downgrade)
//     reason: <text>
//
//     (blank lines allowed, no other line, no control characters);
//   - its author has the admin role on the repository (permission and
//     role_name both "admin"; on a repository of a personal account only
//     the owner has it);
//   - it was never edited (updated_at equals created_at);
//   - with -approval-before, it was created before that time (the merge
//     of the pull request).
//
// author_association, the pull request author and labels are not
// evidence: the workflow requires the formula-rollback label as well,
// but the label alone authorizes nothing. A comment that fails any of
// these is reported (::notice) and ignored, so a comment of someone else
// cannot block the owner's approval, nor stand in for it.

// approvalCommand is the first line of an approval comment.
const approvalCommand = "/formula-rollback"

var (
	sha40Re = regexp.MustCompile(`^[0-9a-f]{40}$`)
	loginRe = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)
)

// approvalSource is where an authorization comes from.
type approvalSource struct {
	flag   bool // -rollback
	login  string
	url    string
	head   string
	reason string
}

func (s approvalSource) String() string {
	if s.flag {
		return "-rollback"
	}
	return fmt.Sprintf("approved by %s in %s for head %s: %s", s.login, s.url, s.head, s.reason)
}

// rollbackAuth is one authorized downgrade.
type rollbackAuth struct {
	path, from, to string
	sources        []approvalSource
	used           bool
}

// rollbacks are the authorized downgrades of a run (nil: none).
type rollbacks struct {
	entries []*rollbackAuth
}

func (r *rollbacks) add(path, from, to string, src approvalSource) {
	for _, e := range r.entries {
		if e.path == path && e.from == from && e.to == to {
			e.sources = append(e.sources, src)
			return
		}
	}
	r.entries = append(r.entries, &rollbackAuth{path: path, from: from, to: to, sources: []approvalSource{src}})
}

// match returns the authorization of exactly this downgrade (nil: none).
func (r *rollbacks) match(path, from, to string) *rollbackAuth {
	if r == nil {
		return nil
	}
	for _, e := range r.entries {
		if e.path == path && e.from == from && e.to == to {
			return e
		}
	}
	return nil
}

func (r *rollbacks) empty() bool { return r == nil || len(r.entries) == 0 }

func (e *rollbackAuth) describe() string {
	s := make([]string, len(e.sources))
	for i, src := range e.sources {
		s[i] = src.String()
	}
	return strings.Join(s, "; ")
}

// parseRollbackFlag parses PATH=FROM->TO.
func parseRollbackFlag(v string) (path, from, to string, err error) {
	i := strings.LastIndex(v, "=")
	if i < 0 {
		return "", "", "", fmt.Errorf("-rollback %q: want PATH=FROM->TO", v)
	}
	path = v[:i]
	from, to, ok := strings.Cut(v[i+1:], "->")
	if !ok {
		return "", "", "", fmt.Errorf("-rollback %q: want PATH=FROM->TO", v)
	}
	if why := checkRollback(path, from, to); why != "" {
		return "", "", "", fmt.Errorf("-rollback %q: %s", v, why)
	}
	return path, from, to, nil
}

// checkRollback says why path, from and to cannot be an authorized
// downgrade ("" when they can).
func checkRollback(path, from, to string) string {
	if why := unsafePath(path); why != "" {
		return fmt.Sprintf("path %q: %s", path, why)
	}
	if !strings.HasSuffix(path, ".rb") || strings.ContainsAny(path, " \t") {
		return fmt.Sprintf("path %q is not a formula file", path)
	}
	if !isSemver(from) || !isSemver(to) {
		return fmt.Sprintf("%q -> %q: not semantic versions", from, to)
	}
	if c, _ := tap.CompareVersions(from, to); c <= 0 {
		return fmt.Sprintf("%s -> %s is not a downgrade", from, to)
	}
	return ""
}

type apiUser struct {
	Login string `json:"login"`
}

// apiComment is the part of an issue comment the approval reads.
type apiComment struct {
	ID        int64    `json:"id"`
	HTMLURL   string   `json:"html_url"`
	Body      *string  `json:"body"`
	CreatedAt string   `json:"created_at"`
	UpdatedAt string   `json:"updated_at"`
	User      *apiUser `json:"user"`
}

// apiPermission is the answer of the collaborator permission endpoint.
type apiPermission struct {
	Permission string   `json:"permission"`
	RoleName   string   `json:"role_name"`
	User       *apiUser `json:"user"`
}

// readJSONStream decodes the JSON values of path: objects, or arrays of
// them (gh api --paginate prints one array per page).
func readJSONStream[T any](path string) ([]T, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	var all []T
	for {
		var raw json.RawMessage
		if err := dec.Decode(&raw); errors.Is(err, io.EOF) {
			return all, nil
		} else if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if s := strings.TrimSpace(string(raw)); strings.HasPrefix(s, "[") {
			var page []T
			if err := json.Unmarshal(raw, &page); err != nil {
				return nil, fmt.Errorf("%s: %w", path, err)
			}
			all = append(all, page...)
			continue
		}
		var v T
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		all = append(all, v)
	}
}

// approvalInput is what -approvals and its companion flags name.
type approvalInput struct {
	comments, permissions string
	head                  string
	before                time.Time // zero: no bound
}

// loadApprovals adds the rollbacks the approval comments authorize; it
// reports every comment it ignores, and why, as a ::notice.
func loadApprovals(in approvalInput, rb *rollbacks, w io.Writer) error {
	comments, err := readJSONStream[apiComment](in.comments)
	if err != nil {
		return err
	}
	perms, err := readJSONStream[apiPermission](in.permissions)
	if err != nil {
		return err
	}
	byLogin := map[string]apiPermission{}
	for _, p := range perms {
		if p.User == nil || p.User.Login == "" {
			return fmt.Errorf("%s: a permission without a user", in.permissions)
		}
		k := strings.ToLower(p.User.Login)
		if old, ok := byLogin[k]; ok && (old.Permission != p.Permission || old.RoleName != p.RoleName) {
			return fmt.Errorf("%s: conflicting permissions of %s", in.permissions, p.User.Login)
		}
		byLogin[k] = p
	}
	sort.SliceStable(comments, func(i, j int) bool { return comments[i].ID < comments[j].ID })
	for _, c := range comments {
		if c.Body == nil || !strings.HasPrefix(*c.Body, approvalCommand) {
			continue // not an approval attempt
		}
		a, why := judgeApproval(c, byLogin, in)
		if why != "" {
			fmt.Fprintf(w, "::notice title=formula-rollback approval ignored::%s\n", escapeData(fmt.Sprintf("comment %s: %s", orUnknown(c.HTMLURL), why)))
			continue
		}
		for _, f := range a.formulae {
			rb.add(f.path, f.from, f.to, a.source)
		}
	}
	return nil
}

func orUnknown(s string) string {
	if s == "" {
		return "(no url)"
	}
	return s
}

type approvedFormula struct{ path, from, to string }

type approval struct {
	source   approvalSource
	formulae []approvedFormula
}

// judgeApproval checks one approval comment; why says why it does not
// authorize anything ("" when it does).
func judgeApproval(c apiComment, perms map[string]apiPermission, in approvalInput) (approval, string) {
	if c.User == nil || !loginRe.MatchString(c.User.Login) {
		return approval{}, "its author is not a user account"
	}
	login := c.User.Login
	p, ok := perms[strings.ToLower(login)]
	if !ok {
		return approval{}, fmt.Sprintf("the permission of %s was not read", login)
	}
	if !strings.EqualFold(p.User.Login, login) {
		return approval{}, "the permission read is of another user"
	}
	if p.Permission != "admin" || p.RoleName != "admin" {
		return approval{}, fmt.Sprintf("%s has %s (role %s) on the repository; an approval needs admin", login, orNone(p.Permission), orNone(p.RoleName))
	}
	if c.CreatedAt == "" || c.UpdatedAt != c.CreatedAt {
		return approval{}, "the comment was edited (updated_at differs from created_at); post a new one"
	}
	if !in.before.IsZero() {
		t, err := time.Parse(time.RFC3339, c.CreatedAt)
		if err != nil {
			return approval{}, fmt.Sprintf("created_at %q: %v", c.CreatedAt, err)
		}
		if !t.Before(in.before) {
			return approval{}, fmt.Sprintf("posted at %s, not before %s (the merge)", c.CreatedAt, in.before.Format(time.RFC3339))
		}
	}
	head, formulae, reason, why := parseApproval(*c.Body)
	if why != "" {
		return approval{}, "malformed: " + why
	}
	if head != in.head {
		return approval{}, fmt.Sprintf("it approves head %s, the pull request head is %s", head, in.head)
	}
	return approval{
		source:   approvalSource{login: login, url: c.HTMLURL, head: head, reason: reason},
		formulae: formulae,
	}, ""
}

// parseApproval parses the body of an approval comment.
func parseApproval(body string) (head string, formulae []approvedFormula, reason, why string) {
	for _, r := range body {
		if (r < 0x20 && r != '\n' && r != '\r' && r != '\t') || r == 0x7f {
			return "", nil, "", "control character"
		}
	}
	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	if strings.TrimRight(lines[0], " \t") != approvalCommand {
		return "", nil, "", "the first line must be " + approvalCommand
	}
	heads, reasons := 0, 0
	seen := map[string]bool{}
	for _, raw := range lines[1:] {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		value = strings.TrimSpace(value)
		switch {
		case !ok:
			return "", nil, "", fmt.Sprintf("line %q is not head:, formula: or reason:", line)
		case key == "head":
			heads++
			if !sha40Re.MatchString(value) {
				return "", nil, "", fmt.Sprintf("head %q is not a 40-digit lowercase commit SHA", value)
			}
			head = value
		case key == "formula":
			f := strings.Fields(value)
			if len(f) != 4 || f[2] != "->" {
				return "", nil, "", fmt.Sprintf("formula %q: want <path> <from> -> <to>", value)
			}
			if w := checkRollback(f[0], f[1], f[3]); w != "" {
				return "", nil, "", "formula " + w
			}
			if seen[f[0]] {
				return "", nil, "", fmt.Sprintf("formula %s named twice", f[0])
			}
			seen[f[0]] = true
			formulae = append(formulae, approvedFormula{f[0], f[1], f[3]})
		case key == "reason":
			reasons++
			if value == "" {
				return "", nil, "", "empty reason"
			}
			reason = value
		default:
			return "", nil, "", fmt.Sprintf("unknown line %q", line)
		}
	}
	switch {
	case heads != 1:
		return "", nil, "", "want exactly one head: line"
	case reasons != 1:
		return "", nil, "", "want exactly one reason: line"
	case len(formulae) == 0:
		return "", nil, "", "want at least one formula: line"
	}
	return head, formulae, reason, ""
}

// finishRollbacks fails every authorization no downgrade used. When
// nothing failed (checkFailed: the check's own findings), it records the
// used ones, the audit trail, in the log and, when set, the step summary.
func finishRollbacks(rb *rollbacks, checkFailed bool, summary string, w io.Writer) (bool, error) {
	if rb.empty() {
		return false, nil
	}
	failed := false
	for _, e := range rb.entries {
		if !e.used {
			annotate(w, "error", e.path, fmt.Sprintf("%s: a rollback from %s to %s is authorized (%s), but the change does not lower %s from %s to %s: the approval must describe the change exactly", e.path, e.from, e.to, e.describe(), e.path, e.from, e.to))
			failed = true
		}
	}
	if failed || checkFailed {
		return failed, nil // refused: no rollback is applied
	}
	var audit strings.Builder
	for _, e := range rb.entries {
		for _, src := range e.sources {
			fmt.Fprintf(w, "::notice title=formula rollback authorized::%s\n", escapeData(fmt.Sprintf("%s %s -> %s %s", e.path, e.from, e.to, src)))
			if !src.flag {
				fmt.Fprintf(&audit, "| %s | %s → %s | %s | %s | `%s` | %s |\n",
					mdCell(e.path), mdCell(e.from), mdCell(e.to), mdCell(src.login), mdCell(src.url), src.head, mdCell(src.reason))
			}
		}
	}
	if summary != "" && audit.Len() > 0 {
		f, err := os.OpenFile(summary, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return failed, err
		}
		_, err = fmt.Fprintf(f, "### Authorized formula rollbacks\n\n| Formula | Version | Approver | Approval | Head | Reason |\n| --- | --- | --- | --- | --- | --- |\n%s\n", audit.String())
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return failed, err
		}
	}
	return failed, nil
}

// mdCell makes s one cell of a Markdown table.
func mdCell(s string) string {
	return strings.NewReplacer("|", `\|`, "\n", " ", "\r", " ", "`", "'", "<", "&lt;", ">", "&gt;").Replace(s)
}
