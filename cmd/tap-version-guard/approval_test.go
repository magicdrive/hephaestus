package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// rollbackTap is a tap at base with change committed on top; it returns
// the repository and the change's commit.
func rollbackTap(t *testing.T, base, change map[string]string) (*tapRepo, string) {
	t.Helper()
	r := newTap(t)
	r.commit(base)
	return r, r.commit(change)
}

// TestRollbackFlag: -rollback lowers exactly the downgrade it names and
// nothing else; an authorization the change does not use fails.
func TestRollbackFlag(t *testing.T) {
	const tool = "tool.rb"
	down := map[string]string{tool: brew("1.5.0")}
	two := map[string]string{tool: brew("2.0.0"), "other.rb": formula("3.0.0")}
	for _, tc := range []struct {
		name         string
		base, change map[string]string
		args         []string
		code         int
		want, not    []string
	}{
		{"downgrade, no authorization", two, down, nil, 1, []string{"::error file=tool.rb::tool.rb: version goes down from 2.0.0 to 1.5.0"}, nil},
		{"authorized downgrade", two, down, []string{"-rollback", "tool.rb=2.0.0->1.5.0"}, 0,
			[]string{"::warning file=tool.rb::tool.rb: version goes down from 2.0.0 to 1.5.0 (authorized rollback: -rollback)", "::notice title=formula rollback authorized::tool.rb 2.0.0 -> 1.5.0 -rollback"},
			[]string{"::error"}},
		{"another target version", two, down, []string{"-rollback", "tool.rb=2.0.0->1.4.0"}, 1,
			[]string{"::error file=tool.rb::tool.rb: version goes down from 2.0.0 to 1.5.0", "a rollback from 2.0.0 to 1.4.0 is authorized (-rollback), but the change does not lower"}, nil},
		{"another source version", two, down, []string{"-rollback", "tool.rb=2.1.0->1.5.0"}, 1,
			[]string{"::error file=tool.rb::tool.rb: version goes down from 2.0.0 to 1.5.0"}, nil},
		{"another formula", two, down, []string{"-rollback", "other.rb=2.0.0->1.5.0"}, 1,
			[]string{"::error file=tool.rb::tool.rb: version goes down", "::error file=other.rb::other.rb: a rollback from 2.0.0 to 1.5.0 is authorized"}, nil},
		{"two downgrades, one authorized", two, map[string]string{tool: brew("1.5.0"), "other.rb": formula("2.0.0")},
			[]string{"-rollback", "tool.rb=2.0.0->1.5.0"}, 1,
			[]string{"::warning file=tool.rb::", "::error file=other.rb::other.rb: version goes down from 3.0.0 to 2.0.0"},
			[]string{"formula rollback authorized"}}, // refused: no audit of an applied rollback
		{"two downgrades, both authorized", two, map[string]string{tool: brew("1.5.0"), "other.rb": formula("2.0.0")},
			[]string{"-rollback", "tool.rb=2.0.0->1.5.0", "-rollback", "other.rb=3.0.0->2.0.0"}, 0, nil, []string{"::error"}},
		{"unused on an upgrade", two, map[string]string{tool: brew("2.1.0")}, []string{"-rollback", "tool.rb=2.0.0->1.5.0"}, 1,
			[]string{"ok tool.rb: 2.0.0 -> 2.1.0", "::error file=tool.rb::tool.rb: a rollback from 2.0.0 to 1.5.0 is authorized"}, nil},
		{"removal is never a rollback", two, map[string]string{tool: ""}, []string{"-rollback", "tool.rb=2.0.0->1.5.0"}, 1,
			[]string{"tool.rb: formula removed (was 2.0.0). A rollback approval never covers a removal"}, nil},
		{"other findings stay errors", map[string]string{tool: brew("2.0.0"), "other.rb": brewFormula("1.0.0", "1111")},
			map[string]string{tool: brew("1.5.0"), "other.rb": brewFormula("1.0.0", "2222")}, []string{"-rollback", "tool.rb=2.0.0->1.5.0"}, 1,
			[]string{"::warning file=tool.rb::", "::error file=other.rb::other.rb: version 1.0.0 is unchanged but its artifacts changed"}, nil},
		{"a url of the old version stays an error", two,
			map[string]string{tool: strings.Replace(brew("1.5.0"), "v1.5.0/tool_1.5.0_darwin_amd64", "v2.0.0/tool_2.0.0_darwin_amd64", 1)},
			[]string{"-rollback", "tool.rb=2.0.0->1.5.0"}, 1, []string{"is of version 2.0.0, the formula's version is 1.5.0"}, nil},
		{"no version on the new side", two, map[string]string{tool: "garbage\n"}, []string{"-rollback", "tool.rb=2.0.0->1.5.0"}, 1,
			[]string{"tool.rb: no version"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, _ := rollbackTap(t, tc.base, tc.change)
			code, out := r.guard(append([]string{"-strict"}, tc.args...)...)
			if code != tc.code {
				t.Fatalf("exit %d, want %d\n%s", code, tc.code, out)
			}
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Errorf("output lacks %q\n%s", w, out)
				}
			}
			for _, n := range tc.not {
				if strings.Contains(out, n) {
					t.Errorf("output has %q\n%s", n, out)
				}
			}
		})
	}
}

// TestRollbackModes: the authorization works in -against and -each too.
func TestRollbackModes(t *testing.T) {
	r := releasePRs(t, "2.0.0", "1.5.0")
	const auth = "Formula/tool.rb=2.0.0->1.5.0"
	if code, out := r.guard("-against", "main", "-head", "tool-1.5.0", "-strict", "-rollback", auth); code != 0 {
		t.Fatalf("-against: exit %d\n%s", code, out)
	}
	if code, out := r.guard("-against", "main", "-head", "tool-1.5.0", "-strict"); code != 1 {
		t.Fatalf("-against without: exit %d\n%s", code, out)
	}
	before := r.git("rev-parse", "main")
	r.git("merge", "-q", "--no-ff", "--no-edit", "tool-1.5.0")
	if code, out := r.guard("-base", before, "-each", "-strict", "-rollback", auth); code != 0 {
		t.Fatalf("-each: exit %d\n%s", code, out)
	}
	if code, out := r.guard("-base", before, "-each", "-strict"); code != 1 {
		t.Fatalf("-each without: exit %d\n%s", code, out)
	}
}

func TestRollbackUsage(t *testing.T) {
	r, head := rollbackTap(t, map[string]string{"tool.rb": brew("2.0.0")}, map[string]string{"tool.rb": brew("1.5.0")})
	dir := t.TempDir()
	c := filepath.Join(dir, "c.json")
	p := filepath.Join(dir, "p.json")
	bad := filepath.Join(dir, "bad.json")
	os.WriteFile(c, []byte("[]"), 0o644)
	os.WriteFile(p, []byte("[]"), 0o644)
	os.WriteFile(bad, []byte("{"), 0o644)
	for _, args := range [][]string{
		{"-rollback", "tool.rb=2.0.0->1.5.0"}, // no -strict
		{"-strict", "-allow-downgrade", "-rollback", "tool.rb=2.0.0->1.5.0"},
		{"-strict", "-rollback", "tool.rb"},
		{"-strict", "-rollback", "tool.rb=2.0.0"},
		{"-strict", "-rollback", "tool.rb=1.5.0->2.0.0"},
		{"-strict", "-rollback", "tool.rb=2.0.0->2.0.0"},
		{"-strict", "-rollback", "../tool.rb=2.0.0->1.5.0"},
		{"-strict", "-rollback", "README.md=2.0.0->1.5.0"},
		{"-strict", "-rollback", "tool.rb=2024.1->2023.1"},
		{"-strict", "-approvals", c},
		{"-strict", "-approvals", c, "-permissions", p},
		{"-strict", "-approvals", c, "-permissions", p, "-approval-head", strings.ToUpper(head)},
		{"-strict", "-permissions", p},
		{"-strict", "-approval-head", head},
		{"-strict", "-approvals", c, "-permissions", p, "-approval-head", head, "-approval-before", "yesterday"},
		{"-strict", "-approvals", bad, "-permissions", p, "-approval-head", head},
		{"-strict", "-approvals", c, "-permissions", bad, "-approval-head", head},
		{"-strict", "-approvals", filepath.Join(dir, "missing"), "-permissions", p, "-approval-head", head},
	} {
		if code, out := r.guard(args...); code != 2 {
			t.Errorf("%v: exit %d, want 2\n%s", args, code, out)
		}
	}
}

const (
	ownerLogin = "magicdrive"
	t0         = "2026-10-27T01:00:00Z"
)

func approvalBody(head string, formulae ...string) string {
	b := "/formula-rollback\nhead: " + head + "\n"
	for _, f := range formulae {
		b += "formula: " + f + "\n"
	}
	return b + "reason: 2.0.0 corrupts the cache (#12)\n"
}

type fakeComment struct {
	id               int
	login            string
	body             string
	created, updated string
}

func (c fakeComment) json() map[string]any {
	if c.created == "" {
		c.created = t0
	}
	if c.updated == "" {
		c.updated = c.created
	}
	return map[string]any{
		"id": c.id, "html_url": "https://github.com/o/tap/pull/7#issuecomment-" + itoa(c.id),
		"body": c.body, "created_at": c.created, "updated_at": c.updated,
		"user": map[string]any{"login": c.login, "type": "User"}, "author_association": "OWNER",
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func perm(login, permission, role string) map[string]any {
	return map[string]any{"permission": permission, "role_name": role, "user": map[string]any{"login": login}}
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestApprovals: which approval comments authorize a rollback. The tap
// lowers tool.rb from 2.0.0 to 1.5.0 at head.
func TestApprovals(t *testing.T) {
	r, head := rollbackTap(t, map[string]string{"tool.rb": brew("2.0.0"), "other.rb": formula("3.0.0")}, map[string]string{"tool.rb": brew("1.5.0")})
	other := strings.Repeat("a", 40)
	good := approvalBody(head, "tool.rb 2.0.0 -> 1.5.0")
	admin := perm(ownerLogin, "admin", "admin")
	for _, tc := range []struct {
		name     string
		comments []fakeComment
		perms    []map[string]any
		before   string
		code     int
		want     []string
	}{
		{"approved by the admin", []fakeComment{{1, ownerLogin, good, "", ""}}, []map[string]any{admin}, "", 0,
			[]string{"authorized rollback: approved by magicdrive in https://github.com/o/tap/pull/7#issuecomment-1 for head " + head + ": 2.0.0 corrupts the cache (#12)"}},
		{"no approval (the label alone)", nil, nil, "", 1, []string{"::error file=tool.rb::tool.rb: version goes down"}},
		{"a comment that is not an approval", []fakeComment{{1, ownerLogin, "LGTM, rollback", "", ""}}, []map[string]any{admin}, "", 1, nil},
		{"write", []fakeComment{{1, "w", good, "", ""}}, []map[string]any{perm("w", "write", "write")}, "", 1,
			[]string{"w has write (role write) on the repository; an approval needs admin"}},
		{"maintain", []fakeComment{{1, "m", good, "", ""}}, []map[string]any{perm("m", "write", "maintain")}, "", 1, []string{"role maintain"}},
		{"triage", []fakeComment{{1, "tr", good, "", ""}}, []map[string]any{perm("tr", "read", "triage")}, "", 1, []string{"role triage"}},
		{"read (a fork author)", []fakeComment{{1, "forker", good, "", ""}}, []map[string]any{perm("forker", "read", "read")}, "", 1, []string{"forker has read"}},
		{"none", []fakeComment{{1, "x", good, "", ""}}, []map[string]any{perm("x", "none", "none")}, "", 1, []string{"x has none"}},
		{"admin permission, custom role", []fakeComment{{1, "c", good, "", ""}}, []map[string]any{perm("c", "admin", "custom")}, "", 1, []string{"role custom"}},
		{"permission not read", []fakeComment{{1, "nobody", good, "", ""}}, []map[string]any{admin}, "", 1, []string{"the permission of nobody was not read"}},
		{"permission login in another case", []fakeComment{{1, ownerLogin, good, "", ""}}, []map[string]any{perm("MagicDrive", "admin", "admin"), perm("x", "read", "read")}, "", 0, nil},
		{"a bot", []fakeComment{{1, "github-actions[bot]", good, "", ""}}, []map[string]any{admin}, "", 1, []string{"its author is not a user account"}},
		{"edited", []fakeComment{{1, ownerLogin, good, t0, "2026-10-27T01:00:05Z"}}, []map[string]any{admin}, "", 1, []string{"the comment was edited"}},
		{"another head (pushed after the approval)", []fakeComment{{1, ownerLogin, approvalBody(other, "tool.rb 2.0.0 -> 1.5.0"), "", ""}}, []map[string]any{admin}, "", 1,
			[]string{"it approves head " + other}},
		{"another formula", []fakeComment{{1, ownerLogin, approvalBody(head, "other.rb 2.0.0 -> 1.5.0"), "", ""}}, []map[string]any{admin}, "", 1,
			[]string{"::error file=tool.rb::tool.rb: version goes down", "::error file=other.rb::other.rb: a rollback from 2.0.0 to 1.5.0 is authorized (approved by magicdrive"}},
		{"another version", []fakeComment{{1, ownerLogin, approvalBody(head, "tool.rb 2.0.0 -> 1.4.0"), "", ""}}, []map[string]any{admin}, "", 1,
			[]string{"::error file=tool.rb::tool.rb: version goes down"}},
		{"another source version", []fakeComment{{1, ownerLogin, approvalBody(head, "tool.rb 2.1.0 -> 1.5.0"), "", ""}}, []map[string]any{admin}, "", 1, nil},
		{"a second formula approved but unchanged", []fakeComment{{1, ownerLogin, approvalBody(head, "tool.rb 2.0.0 -> 1.5.0", "other.rb 3.0.0 -> 2.0.0"), "", ""}}, []map[string]any{admin}, "", 1,
			[]string{"::warning file=tool.rb::", "::error file=other.rb::other.rb: a rollback from 3.0.0 to 2.0.0 is authorized"}},
		{"before the merge", []fakeComment{{1, ownerLogin, good, "", ""}}, []map[string]any{admin}, "2026-10-27T02:00:00Z", 0, nil},
		{"after the merge", []fakeComment{{1, ownerLogin, good, "", ""}}, []map[string]any{admin}, "2026-10-27T00:59:59Z", 1, []string{"not before 2026-10-27T00:59:59Z (the merge)"}},
		{"someone else's approval does not block the admin's", []fakeComment{{1, "forker", good, "", ""}, {2, ownerLogin, good, "", ""}},
			[]map[string]any{perm("forker", "read", "read"), admin}, "", 0, []string{"forker has read"}},
		{"two admin approvals of other targets", []fakeComment{{1, ownerLogin, good, "", ""}, {2, ownerLogin, approvalBody(head, "tool.rb 2.0.0 -> 1.4.0"), "", ""}},
			[]map[string]any{admin}, "", 1, []string{"a rollback from 2.0.0 to 1.4.0 is authorized"}},
		{"the same approval twice", []fakeComment{{1, ownerLogin, good, "", ""}, {2, ownerLogin, good, "", ""}}, []map[string]any{admin}, "", 0, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			var cs []map[string]any
			for _, c := range tc.comments {
				cs = append(cs, c.json())
			}
			// Two pages, as gh api --paginate prints them.
			cpath, ppath, summary := filepath.Join(dir, "c.json"), filepath.Join(dir, "p.json"), filepath.Join(dir, "summary.md")
			if len(cs) > 1 {
				a, _ := json.Marshal(cs[:1])
				b, _ := json.Marshal(cs[1:])
				os.WriteFile(cpath, append(append(a, '\n'), b...), 0o644)
			} else {
				writeJSON(t, cpath, append([]map[string]any{}, cs...))
			}
			var ps []byte
			for _, p := range tc.perms {
				b, _ := json.MarshalIndent(p, "", "  ")
				ps = append(append(ps, b...), '\n')
			}
			os.WriteFile(ppath, ps, 0o644)
			args := []string{"-strict", "-approvals", cpath, "-permissions", ppath, "-approval-head", head, "-summary", summary}
			if tc.before != "" {
				args = append(args, "-approval-before", tc.before)
			}
			code, out := r.guard(args...)
			if code != tc.code {
				t.Fatalf("exit %d, want %d\n%s", code, tc.code, out)
			}
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Errorf("output lacks %q\n%s", w, out)
				}
			}
			sum, _ := os.ReadFile(summary)
			if code == 0 {
				for _, w := range []string{"### Authorized formula rollbacks", "| tool.rb | 2.0.0 → 1.5.0 | magicdrive | https://github.com/o/tap/pull/7#issuecomment-", "| `" + head + "` | 2.0.0 corrupts the cache (#12) |"} {
					if !strings.Contains(string(sum), w) {
						t.Errorf("summary lacks %q\n%s", w, sum)
					}
				}
				if !strings.Contains(out, "::notice title=formula rollback authorized::tool.rb 2.0.0 -> 1.5.0 approved by magicdrive") {
					t.Errorf("no audit notice\n%s", out)
				}
			}
		})
	}
}

// TestParseApproval: the comment format is strict.
func TestParseApproval(t *testing.T) {
	head := strings.Repeat("0123456789", 4)
	ok := []string{
		approvalBody(head, "tool.rb 2.0.0 -> 1.5.0"),
		approvalBody(head, "Formula/t/tool.rb 1.3.0 -> 1.3.0-rc.1", "other.rb v2.0.0 -> v1.0.0"),
		"/formula-rollback\r\n\r\nhead: " + head + "\r\nformula: tool.rb 2.0.0 -> 1.5.0\r\nreason: a: b\r\n",
		"/formula-rollback  \n  head:   " + head + "\nformula:  tool.rb   2.0.0   ->   1.5.0\nreason: x\n\n",
	}
	for _, b := range ok {
		if _, _, _, why := parseApproval(b); why != "" {
			t.Errorf("%q: %s", b, why)
		}
	}
	bad := map[string]string{
		"no command":           "head: " + head + "\nformula: tool.rb 2.0.0 -> 1.5.0\nreason: x\n",
		"command with text":    "/formula-rollback please\nhead: " + head + "\nformula: tool.rb 2.0.0 -> 1.5.0\nreason: x\n",
		"command later":        "please\n/formula-rollback\nhead: " + head + "\nformula: tool.rb 2.0.0 -> 1.5.0\nreason: x\n",
		"no head":              "/formula-rollback\nformula: tool.rb 2.0.0 -> 1.5.0\nreason: x\n",
		"two heads":            "/formula-rollback\nhead: " + head + "\nhead: " + head + "\nformula: tool.rb 2.0.0 -> 1.5.0\nreason: x\n",
		"short head":           "/formula-rollback\nhead: 0123456\nformula: tool.rb 2.0.0 -> 1.5.0\nreason: x\n",
		"upper-case head":      "/formula-rollback\nhead: " + strings.ToUpper(strings.Repeat("abcdef0123", 4)) + "\nformula: tool.rb 2.0.0 -> 1.5.0\nreason: x\n",
		"no formula":           "/formula-rollback\nhead: " + head + "\nreason: x\n",
		"no reason":            "/formula-rollback\nhead: " + head + "\nformula: tool.rb 2.0.0 -> 1.5.0\n",
		"empty reason":         "/formula-rollback\nhead: " + head + "\nformula: tool.rb 2.0.0 -> 1.5.0\nreason:\n",
		"two reasons":          "/formula-rollback\nhead: " + head + "\nformula: tool.rb 2.0.0 -> 1.5.0\nreason: x\nreason: y\n",
		"not a downgrade":      approvalBody(head, "tool.rb 1.5.0 -> 2.0.0"),
		"same version":         approvalBody(head, "tool.rb 1.5.0 -> 1.5.0"),
		"not semantic":         approvalBody(head, "tool.rb 2024.1 -> 2023.1"),
		"no arrow":             approvalBody(head, "tool.rb 2.0.0 1.5.0"),
		"extra field":          approvalBody(head, "tool.rb 2.0.0 -> 1.5.0 now"),
		"unsafe path":          approvalBody(head, "../tool.rb 2.0.0 -> 1.5.0"),
		"not a formula file":   approvalBody(head, "tool 2.0.0 -> 1.5.0"),
		"formula named twice":  approvalBody(head, "tool.rb 2.0.0 -> 1.5.0", "tool.rb 2.0.0 -> 1.4.0"),
		"unknown line":         "/formula-rollback\nhead: " + head + "\nformula: tool.rb 2.0.0 -> 1.5.0\nreason: x\napproved: yes\n",
		"free text":            "/formula-rollback\nhead: " + head + "\nformula: tool.rb 2.0.0 -> 1.5.0\nreason: x\nthanks\n",
		"workflow command":     "/formula-rollback\nhead: " + head + "\nformula: tool.rb 2.0.0 -> 1.5.0\nreason: x\n::add-mask::y\n",
		"control character":    "/formula-rollback\nhead: " + head + "\nformula: tool.rb 2.0.0 -> 1.5.0\nreason: x\x1b[2J\n",
		"key with a space":     "/formula-rollback\nhead : " + head + "\nformula: tool.rb 2.0.0 -> 1.5.0\nreason: x\n",
		"empty":                "",
		"case of the command":  "/Formula-Rollback\nhead: " + head + "\nformula: tool.rb 2.0.0 -> 1.5.0\nreason: x\n",
		"case of a key":        "/formula-rollback\nHead: " + head + "\nformula: tool.rb 2.0.0 -> 1.5.0\nreason: x\n",
		"leading space":        " /formula-rollback\nhead: " + head + "\nformula: tool.rb 2.0.0 -> 1.5.0\nreason: x\n",
		"control in the path":  approvalBody(head, "to\x7fol.rb 2.0.0 -> 1.5.0"),
		"formula with a space": "/formula-rollback\nhead: " + head + "\nformula: my tool.rb 2.0.0 -> 1.5.0\nreason: x\n",
	}
	for name, b := range bad {
		if _, _, _, why := parseApproval(b); why == "" {
			t.Errorf("%s: %q parsed", name, b)
		}
	}
}

// TestApprovalEscapes: what a commenter writes reaches the log escaped;
// it cannot start a workflow command of its own.
func TestApprovalEscapes(t *testing.T) {
	r, head := rollbackTap(t, map[string]string{"tool.rb": brew("2.0.0")}, map[string]string{"tool.rb": brew("1.5.0")})
	dir := t.TempDir()
	cpath, ppath := filepath.Join(dir, "c.json"), filepath.Join(dir, "p.json")
	body := "/formula-rollback\nhead: " + head + "\nformula: tool.rb 2.0.0 -> 1.5.0\nreason: %0A::error::x |<b>|`y`\n"
	writeJSON(t, cpath, []map[string]any{fakeComment{1, ownerLogin, body, "", ""}.json(), fakeComment{2, "w", "/formula-rollback\n::add-mask::z\r\n::error::spoof", "", ""}.json()})
	writeJSON(t, ppath, []map[string]any{perm(ownerLogin, "admin", "admin"), perm("w", "write", "write")})
	summary := filepath.Join(dir, "s.md")
	code, out := r.guard("-strict", "-approvals", cpath, "-permissions", ppath, "-approval-head", head, "-summary", summary)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "::error") || strings.HasPrefix(line, "::add-mask") || strings.Contains(line, "\r") {
			t.Errorf("injected line %q", line)
		}
	}
	if !strings.Contains(out, "reason: %250A::error::x") && !strings.Contains(out, ": %250A::error::x") {
		t.Errorf("reason not escaped\n%s", out)
	}
	sum, _ := os.ReadFile(summary)
	if strings.Contains(string(sum), "<b>") || strings.Contains(string(sum), "`y`") || strings.Count(string(sum), "|<") > 0 {
		t.Errorf("summary not escaped\n%s", sum)
	}
}
