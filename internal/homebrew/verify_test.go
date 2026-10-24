package homebrew

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/magicdrive/hephaestus/internal/manifest"
)

func checkRubySyntax(t *testing.T, content []byte) {
	t.Helper()
	ruby := requireRuby(t)
	p := filepath.Join(t.TempDir(), "formula.rb")
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(ruby, "-c", p).CombinedOutput(); err != nil {
		t.Fatalf("ruby -c: %v\n%s\n%s", err, out, content)
	}
}

func TestVerifyMatch(t *testing.T) {
	files := append(stdFiles(), file("NOTES.md", modeRegular), file("CHANGES.md", modeRegular), file("AUTHORS", modeRegular), file("mycli2src", modeExec))
	var arts []manifest.Artifact
	for _, a := range fourPlatforms(false) {
		arts = append(arts, arch(a.OS, a.Arch, "", files, "mycli"))
	}
	// Every argument form, each file installed once.
	w, err := verifyInstall("mycli", `bin.install "mycli"
prefix.install "README.md", "LICENSE"
doc.install ["NOTES.md"]
pkgshare.install %w[CHANGES.md AUTHORS]
bin.install("mycli2src" => "mycli2")`, arts)
	if err != nil || len(w) != 0 {
		t.Errorf("warnings %v, err %v", w, err)
	}
}

func TestVerifyMissing(t *testing.T) {
	arts := fourPlatforms(false)
	_, err := verifyInstall("mycli", "bin.install \"mycli\"\nman1.install \"man/mycli.1\"\nbash_completion.install \"completions/mycli.bash\" => \"mycli\"", arts)
	if err == nil {
		t.Fatal("missing paths accepted")
	}
	msg := err.Error()
	for _, a := range arts {
		if !strings.Contains(msg, a.Name+`: "man/mycli.1" (man1.install, install line 2) not found`) {
			t.Errorf("error does not name %s/man: %s", a.Name, msg)
		}
		if !strings.Contains(msg, a.Name+`: "completions/mycli.bash" (bash_completion.install, install line 3) not found`) {
			t.Errorf("error does not name %s/completions: %s", a.Name, msg)
		}
	}
	if strings.Contains(msg, `"mycli" (bin.install`) {
		t.Errorf("present path reported: %s", msg)
	}
	_, err = verifyInstall("mycli", `bin.install "../etc/passwd"`, arts)
	if err == nil || !strings.Contains(err.Error(), "outside the archive") {
		t.Errorf("err = %v", err)
	}
}

func TestVerifyPerArchiveLayout(t *testing.T) {
	// darwin archives are wrapped in a top-level directory, linux ones are
	// not; only linux/arm64 ships the man page.
	arts := []manifest.Artifact{
		arch("darwin", "arm64", "mycli_darwin_arm64", stdFiles(), "mycli"),
		arch("darwin", "amd64", "mycli_darwin_amd64", stdFiles(), "mycli"),
		arch("linux", "amd64", "", stdFiles(), "mycli"),
		arch("linux", "arm64", "", append(stdFiles(), file("mycli.1", modeRegular)), "mycli"),
	}
	if _, err := verifyInstall("mycli", `bin.install "mycli"`, arts); err != nil {
		t.Errorf("single top-level directory must be stripped: %v", err)
	}
	_, err := verifyInstall("mycli", "bin.install \"mycli\"\nman1.install \"mycli.1\"", arts)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	for _, n := range []string{arts[0].Name, arts[1].Name, arts[2].Name} {
		if !strings.Contains(msg, n+`: "mycli.1"`) {
			t.Errorf("error does not name %s: %s", n, msg)
		}
	}
	if strings.Contains(msg, arts[3].Name) {
		t.Errorf("archive with the file reported: %s", msg)
	}
	if !strings.Contains(msg, `relative to the top-level directory "mycli_darwin_arm64"`) {
		t.Errorf("no hint about the stripped directory: %s", msg)
	}
	// Referencing the wrapper directory itself fails on wrapped archives.
	if _, err := verifyInstall("mycli", `bin.install "mycli_darwin_arm64/mycli"`, arts[:1]); err == nil {
		t.Error("path including the stripped directory accepted")
	}
}

func TestVerifyTopLevelRule(t *testing.T) {
	// Dotfiles do not count (Dir["*"]), so the single directory is entered.
	a := arch("linux", "amd64", "", []manifest.File{file(".hidden", modeRegular), file("d/", ""), file("d/mycli", modeExec)})
	if _, err := verifyInstall("x", `bin.install "mycli"`, []manifest.Artifact{a}); err != nil {
		t.Errorf("dotfile prevented stripping: %v", err)
	}
	// Two top-level entries: nothing is stripped.
	a = arch("linux", "amd64", "", []manifest.File{file("d/mycli", modeExec), file("README", modeRegular)})
	if _, err := verifyInstall("x", `bin.install "d/mycli"`, []manifest.Artifact{a}); err != nil {
		t.Errorf("err = %v", err)
	}
	// A single top-level file is not a directory.
	a = arch("linux", "amd64", "", []manifest.File{file("mycli", modeExec)})
	if _, err := verifyInstall("x", `bin.install "mycli"`, []manifest.Artifact{a}); err != nil {
		t.Errorf("err = %v", err)
	}
	// Implicit directory (no dir entry) is still a directory.
	a = arch("linux", "amd64", "", []manifest.File{file("top/bin/mycli", modeExec)})
	if _, err := verifyInstall("x", `bin.install "bin/mycli"`, []manifest.Artifact{a}); err != nil {
		t.Errorf("err = %v", err)
	}
	if _, err := verifyInstall("x", `prefix.install "bin"`, []manifest.Artifact{a}); err != nil {
		t.Errorf("directory install: %v", err)
	}
}

func TestVerifyConditions(t *testing.T) {
	arts := []manifest.Artifact{
		arch("darwin", "arm64", "", []manifest.File{file("mycli-mac", modeExec)}),
		arch("linux", "amd64", "", []manifest.File{file("mycli-linux", modeExec)}),
	}
	// Each block alone (one archive installs each file once).
	for _, code := range []string{
		"bin.install \"mycli-mac\" => \"mycli\" if OS.mac?\nbin.install \"mycli-linux\" => \"mycli\" unless OS.mac?",
		"if OS.mac?\n  bin.install \"mycli-mac\"\nelse\n  bin.install \"mycli-linux\"\nend",
	} {
		if _, err := verifyInstall("x", code, arts); err != nil {
			t.Errorf("%s: err = %v", code, err)
		}
	}
	code := `if OS.linux? && Hardware::CPU.intel?
  bin.install "mycli-linux"
elsif OS.mac?
  bin.install "unknowable"
else
  bin.install "also-unknowable"
end`
	w, err := verifyInstall("x", code, arts)
	if err != nil {
		t.Errorf("err = %v", err)
	}
	if n := countContaining(w, "cannot verify"); n != 2 {
		t.Errorf("want 2 cannot-verify warnings (elsif/else), got %v", w)
	}
	_, err = verifyInstall("x", `bin.install "mycli-mac" if OS.linux?`, arts)
	if err == nil || !strings.Contains(err.Error(), arts[1].Name) || strings.Contains(err.Error(), arts[0].Name) {
		t.Errorf("err = %v", err)
	}
}

func TestVerifyDynamic(t *testing.T) {
	arts := fourPlatforms(false)
	code := `bin.install Dir["*"]
(prefix/"x").install "mycli"
bin.install "#{name}"
bin.install name
libexec.install "a", Dir["b/*"]
cd "sub" do
  bin.install "inside-block"
end
bin.install_symlink libexec/"mycli"
bin.install "mycli" if ENV["X"]
bin.install
generate_completions_from_executable(bin/"mycli", "completion")`
	w, err := verifyInstall("mycli", code, arts)
	if err != nil {
		t.Errorf("dynamic calls must not fail verification: %v", err)
	}
	if n := countContaining(w, "cannot verify"); n != 8 {
		t.Errorf("want 8 cannot-verify warnings, got %d:\n%s", n, strings.Join(w, "\n"))
	}
}

func TestVerifyExecutableBit(t *testing.T) {
	a := arch("linux", "amd64", "", []manifest.File{file("mycli", modeRegular)})
	w, err := verifyInstall("x", `bin.install "mycli"`, []manifest.Artifact{a})
	if err != nil || countContaining(w, "not executable") != 1 {
		t.Errorf("warnings %v, err %v", w, err)
	}
}

func TestVerifyIgnoresStringsAndComments(t *testing.T) {
	arts := fourPlatforms(false)
	code := `# bin.install "nope"
ohai "bin.install \"nope\""
x = <<~EOS
  bin.install "nope"
EOS
bin.install "mycli" # trailing comment`
	if w, err := verifyInstall("x", code, arts); err != nil || len(w) != 0 {
		t.Errorf("warnings %v, err %v", w, err)
	}
}

func TestVerifyFormula(t *testing.T) {
	b := baseBrew()
	b.ExtraInstall = `man1.install "man/mycli.1"`
	_, err := Generate(input(b, fourPlatforms(false)...))
	if err == nil || !strings.Contains(err.Error(), "man/mycli.1") {
		t.Fatalf("Generate err = %v", err)
	}
	// verify: false disables the check, Verify still reports it.
	off := false
	b.Verify = &off
	f := generateOne(t, input(b, fourPlatforms(false)...))
	if err := Verify(f); err == nil || !strings.Contains(err.Error(), "man/mycli.1") {
		t.Errorf("Verify err = %v", err)
	}
	if err := Verify(Formula{Content: []byte("class X < Formula\nend\n")}); err == nil {
		t.Error("formula without install accepted")
	}
}

func TestGenerateDynamicInstallWarns(t *testing.T) {
	b := baseBrew()
	b.Install = "libexec.install Dir[\"*\"]\nbin.install_symlink libexec/\"mycli\""
	b.Test = `assert_predicate bin/"mycli", :executable?`
	f := generateOne(t, input(b, fourPlatforms(false)...))
	if countContaining(f.Warnings, "cannot verify") != 1 {
		t.Errorf("warnings = %v", f.Warnings)
	}
}

func countContaining(list []string, s string) int {
	n := 0
	for _, x := range list {
		if strings.Contains(x, s) {
			n++
		}
	}
	return n
}

// FuzzAnalyzeInstall checks that the install analyzer and the code block
// checks never panic or hang on arbitrary input.
func FuzzAnalyzeInstall(f *testing.F) {
	for _, s := range []string{
		`bin.install "a" => "b"`, `if OS.mac? && !Hardware::CPU.arm?` + "\n" + `bin.install %w[a b]` + "\nend",
		"x = <<~EOS\nbin.install \"x\"\nEOS", `bin.install("a", ["b", "c"])`, `"#{`, `%w[`, "if !", "<<~", `bin.install "\`,
	} {
		f.Add(s)
	}
	arts := fourPlatforms(true)
	f.Fuzz(func(t *testing.T, code string) {
		_ = analyzeInstall(code)
		_, _ = verifyInstall("x", code, arts)
		_, _, _ = codeBlock("install", code, "    ")
	})
}

// TestVerifyDuplicateInstalls: installs that cannot all succeed, or that
// overwrite each other, are errors (Pathname#install moves its source).
func TestVerifyDuplicateInstalls(t *testing.T) {
	files := []manifest.File{file("t", modeExec), file("t.1", modeRegular), file("misc/", ""), file("misc/completions/", ""),
		file("misc/completions/t-completion.sh", modeRegular), file("other", modeExec)}
	arts := []manifest.Artifact{arch("darwin", "arm64", "", files, "t"), arch("linux", "amd64", "", files, "t")}
	for _, tc := range []struct {
		name, code, want string
	}{
		// magicdrive/t's first formula: the second install found nothing.
		{"same source twice", "bin.install \"t\"\nbash_completion.install \"misc/completions/t-completion.sh\" => \"t\"\npkgshare.install \"misc/completions/t-completion.sh\"", "is installed twice"},
		{"same destination", "bin.install \"t\"\nbin.install \"other\" => \"t\"", "two installs write bin/t"},
		{"man page twice", "man1.install \"t.1\"\nman1.install \"t.1\"", "is installed twice"},
		{"directory and a file in it", "pkgshare.install \"misc\"\nbash_completion.install \"misc/completions/t-completion.sh\"", "overlaps"},
		{"zsh and fish from one file", "zsh_completion.install \"misc/completions/t-completion.sh\" => \"_t\"\nfish_completion.install \"misc/completions/t-completion.sh\"", "is installed twice"},
		{"only on one platform", "bin.install \"t\"\nbin.install \"t\" if OS.linux?", arts[1].Name},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := verifyInstall("t", tc.code, arts)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
		})
	}
	// The fixed formula: install once, link the completion.
	ok := "bin.install \"t\"\nman1.install \"t.1\"\npkgshare.install \"misc/completions/t-completion.sh\"\nbash_completion.install_symlink pkgshare/\"t-completion.sh\" => \"t\""
	if _, err := verifyInstall("t", ok, arts); err != nil {
		t.Errorf("fixed formula: %v", err)
	}
	// Platform-exclusive installs of one name are fine.
	excl := "bin.install \"t\" if OS.mac?\nbin.install \"other\" => \"t\" unless OS.mac?"
	if _, err := verifyInstall("t", excl, arts); err != nil {
		t.Errorf("platform-exclusive: %v", err)
	}
}
