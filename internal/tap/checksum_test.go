package tap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/magicdrive/hephaestus/internal/config"
)

// --- Same version, other checksums ---

const (
	sumA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	sumB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	sumC = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

func darwinURL(version string) string {
	return "https://github.com/acme/tool/releases/download/v" + version + "/tool_" + version + "_darwin_arm64.tar.gz"
}

func linuxURL(version string) string {
	return "https://github.com/acme/tool/releases/download/v" + version + "/tool_" + version + "_linux_amd64.tar.gz"
}

// formulaSums is a generated-style formula of version with one archive per
// platform: darwin's sha256 is darwin, linux's is linux ("" leaves linux
// out). desc varies what is not a checksum.
func formulaSums(version, darwin, linux, desc string) string {
	var b strings.Builder
	b.WriteString("class Tool < Formula\n  desc \"" + desc + "\"\n  version \"" + version + "\"\n\n")
	b.WriteString("  on_macos do\n    on_arm do\n      url \"" + darwinURL(version) + "\"\n      sha256 \"" + darwin + "\"\n    end\n  end\n")
	if linux != "" {
		b.WriteString("\n  on_linux do\n    on_intel do\n      url \"" + linuxURL(version) + "\"\n      sha256 \"" + linux + "\"\n    end\n  end\n")
	}
	b.WriteString("\n  def install\n    bin.install \"tool\"\n  end\nend\n")
	return b.String()
}

// sumsInput is releaseInput with the formula content given.
func sumsInput(t *testing.T, f *fakeGitHub, version, content string, mutate ...func(*config.Brew)) Input {
	t.Helper()
	in := releaseInput(t, f, version, mutate...)
	rel := in.Manifest.Artifacts[0].Path
	if err := os.WriteFile(filepath.Join(in.Config.Root, filepath.FromSlash(rel)), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	in.Manifest.Artifacts[0].SHA256 = sum(content)
	in.Manifest.Artifacts[0].Size = int64(len(content))
	return in
}

func TestChecksumChanges(t *testing.T) {
	v := "1.2.0"
	base := formulaSums(v, sumA, sumB, "tool")
	for _, tc := range []struct {
		name     string
		old, new string
		want     []string
	}{
		{"identical", base, base, nil},
		{"only desc changed", base, formulaSums(v, sumA, sumB, "a better tool"), nil},
		{"one archive replaced", base, formulaSums(v, sumC, sumB, "tool"), []string{darwinURL(v)}},
		{"both replaced", base, formulaSums(v, sumC, sumC, "tool"), []string{darwinURL(v), linuxURL(v)}},
		{"upper-case hex is the same checksum", base, strings.Replace(base, sumA, strings.ToUpper(sumA), 1), nil},
		{"platform added", formulaSums(v, sumA, "", "tool"), base, nil},
		{"platform removed", base, formulaSums(v, sumA, "", "tool"), nil},
		{"new version", base, formulaSums("1.3.0", sumC, sumC, "tool"), nil},
		{"same precedence (build metadata)", base, strings.Replace(formulaSums(v, sumC, sumB, "tool"), `version "1.2.0"`, `version "1.2.0+build.7"`, 1), []string{darwinURL(v)}},
		{"no version on either side", "class Tool < Formula\n  url \"https://example.com/tool.tgz\"\n  sha256 \"" + sumA + "\"\nend\n",
			"class Tool < Formula\n  url \"https://example.com/tool.tgz\"\n  sha256 \"" + sumB + "\"\nend\n", nil},
		{"version from the url", "class Tool < Formula\n  url \"" + darwinURL(v) + "\"\n  sha256 \"" + sumA + "\"\nend\n",
			"class Tool < Formula\n  url \"" + darwinURL(v) + "\"\n  sha256 \"" + sumB + "\"\nend\n", []string{darwinURL(v)}},
		{"resource archive replaced", strings.Replace(base, "  def install", "  resource \"extra\" do\n    url \"https://example.com/extra.tgz\"\n    sha256 \""+sumA+"\"\n  end\n\n  def install", 1),
			strings.Replace(base, "  def install", "  resource \"extra\" do\n    url \"https://example.com/extra.tgz\"\n    sha256 \""+sumB+"\"\n  end\n\n  def install", 1), []string{"https://example.com/extra.tgz"}},
		{"heredoc lines do not count", strings.Replace(base, "  def install", "  def caveats\n    <<~EOS\n      url \""+darwinURL(v)+"\"\n      sha256 \""+sumC+"\"\n    EOS\n  end\n\n  def install", 1), base, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ChecksumChanges([]byte(tc.old), []byte(tc.new)); !slices.Equal(got, tc.want) {
				t.Fatalf("ChecksumChanges = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSameVersionOtherChecksumsIsRefused: wherever the tap already has (or
// is proposed) the version being published with other checksums for the
// same archives, the tap stage fails before writing anything, in a dry run
// (the rehearsal before the release is published) as well. allow_downgrade
// does not change that.
func TestSameVersionOtherChecksumsIsRefused(t *testing.T) {
	const v = "1.2.0"
	published := formulaSums(v, sumA, sumB, "tool")
	replaced := formulaSums(v, sumC, sumB, "tool")
	for _, where := range []string{"base", "direct", "head branch", "open pull request"} {
		for _, dry := range []bool{false, true} {
			for _, allow := range []bool{false, true} {
				name := where
				if dry {
					name += "/dry-run"
				}
				if allow {
					name += "/allow_downgrade"
				}
				t.Run(name, func(t *testing.T) {
					f := newFake(t, token)
					files := map[string]string{formulaPath: formulaWith("1.1.0")}
					if where == "base" || where == "direct" {
						files[formulaPath] = published
					}
					f.addRepo(tapRepo, "main", files)
					switch where {
					case "head branch":
						// An earlier run of this tag left its branch.
						f.mu.Lock()
						f.repos[tapRepo].branches["tool-"+v] = f.repos[tapRepo].branches["main"]
						f.setFileLocked(tapRepo, "tool-"+v, formulaPath, published)
						f.mu.Unlock()
					case "open pull request":
						f.openRivalPull(tapRepo, "main", "rival", formulaPath, published)
					}
					var mutate []func(*config.Brew)
					if where == "direct" {
						mutate = append(mutate, func(b *config.Brew) {
							b.Repository.PullRequest = &config.BrewPullRequest{Enabled: boolPtr(false)}
						})
					}
					if allow {
						mutate = append(mutate, allowDowngrade)
					}
					in := sumsInput(t, f, v, replaced, mutate...)
					in.DryRun = dry
					f.resetLog()
					res, err := Run(context.Background(), in)
					var ce *ChecksumConflictError
					if !errors.As(err, &ce) || len(res) != 1 || res[0].Status != StatusFailed || res[0].NoOp {
						t.Fatalf("err %v, results %+v", err, res)
					}
					if !slices.Equal(ce.URLs, []string{darwinURL(v)}) || ce.Version != v ||
						!strings.Contains(err.Error(), "other sha256 checksums") || !strings.Contains(err.Error(), "nothing was written") {
						t.Fatalf("error %v", err)
					}
					if w := f.writes(); len(w) != 0 {
						t.Fatalf("writes %v", w)
					}
				})
			}
		}
	}
}

// TestSameVersionSameChecksumsIsUpdated: a formula of the same version
// whose checksums are unchanged (a newer Hephaestus changed desc, caveats
// or install, or added a platform) is still proposed.
func TestSameVersionSameChecksumsIsUpdated(t *testing.T) {
	const v = "1.2.0"
	for name, proposed := range map[string]string{
		"desc":           formulaSums(v, sumA, sumB, "a better tool"),
		"platform added": formulaSums(v, sumA, sumB, "tool"),
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t, token)
			onBase := formulaSums(v, sumA, sumB, "tool")
			if name == "platform added" {
				onBase = formulaSums(v, sumA, "", "tool")
			}
			f.addRepo(tapRepo, "main", map[string]string{formulaPath: onBase})
			r := one(t)(Run(context.Background(), sumsInput(t, f, v, proposed)))
			if r.Status != StatusUpdated || r.PullRequestURL == "" {
				t.Fatalf("%+v", r)
			}
		})
	}
}

func boolPtr(b bool) *bool { return &b }

// openForkPull opens a pull request against acme/homebrew-tap from the
// fork owner/homebrew-tap, whose branch changes path to content: anyone can
// open one on a public tap.
func (f *fakeGitHub) openForkPull(owner, branch, path, content string) {
	fork := owner + "/homebrew-tap"
	f.addRepo(fork, "main", map[string]string{})
	f.mu.Lock()
	defer f.mu.Unlock()
	f.repos[fork].branches[branch] = f.repos[tapRepo].branches["main"]
	f.setFileLocked(fork, branch, path, content)
	f.addPullLocked(tapRepo, owner+":"+branch, branch, "main")
}

// TestForkPullDoesNotBlockPublishing: an open pull request from another
// repository (a fork; anyone can open one on a public tap) is not a release
// of this project. One proposing a newer version, or this version with
// other checksums, is reported as a warning; it neither skips nor fails the
// tap stage, so an outsider cannot stop the tap from being updated. A pull
// request from the tap itself still does (TestNewerOpenPullBlocksOlder,
// TestSameVersionOtherChecksumsIsRefused).
func TestForkPullDoesNotBlockPublishing(t *testing.T) {
	const v = "1.2.0"
	for _, tc := range []struct{ name, fork, warn string }{
		{"newer version", formulaWith("99.0.0"), "from mallory:bump, not a branch of acme/homebrew-tap, proposes version 99.0.0"},
		{"same version, other checksums", formulaSums(v, sumC, sumB, "tool"), "from mallory:bump, not a branch of acme/homebrew-tap, proposes version 1.2.0 of Formula/tool.rb with other sha256 checksums"},
	} {
		for _, dry := range []bool{false, true} {
			name := tc.name
			if dry {
				name += "/dry-run"
			}
			t.Run(name, func(t *testing.T) {
				f := newFake(t, token)
				f.addRepo(tapRepo, "main", map[string]string{formulaPath: formulaWith("1.1.0")})
				f.openForkPull("mallory", "bump", formulaPath, tc.fork)
				in := sumsInput(t, f, v, formulaSums(v, sumA, sumB, "tool"))
				in.DryRun = dry
				r := one(t)(Run(context.Background(), in))
				want := StatusUpdated
				if dry {
					want = StatusPlanned
				}
				if r.Status != want || !strings.Contains(warnings(r), tc.warn) {
					t.Fatalf("%+v", r)
				}
			})
		}
	}
}
